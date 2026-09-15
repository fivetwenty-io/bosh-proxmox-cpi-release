// Proof that a volume is no longer on storage, for the callers that have to
// fail closed when they cannot establish it: the delete_disk parked-anchor
// refusal, the has_disk answer bosh cck reads, the stale-slot check in
// delete_vm, and the local-backend cluster scan.
package pve

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// StorageClassifier answers what kind of storage we are proving against. It is
// consulted only after the point probe has failed and we are about to read a
// listing, so the fast paths pay nothing for it. A false return means the
// storage could not be identified, which is not a license to guess.
type StorageClassifier func(context.Context) (StorageInfo, bool)

// ProveVolumeAbsent reports whether volume is provably not on storage, as
// observed from node. (true, nil) means the volume is not there. (false, nil)
// means it is. A non-nil error means the observation did not land and the
// caller has to fail closed, because nothing about the volume has been
// established.
//
// The volume argument is a full volid such as "nfs-images:9000/vm-9000-disk-0.qcow2",
// and storage names the storage it lives on.
//
// The corroborators are optional and are consulted in the one case where an
// empty listing is about to be read as an absence: an nfs or cifs export that
// mounts but serves the wrong tree lists nothing while its real content sits
// elsewhere, and the allow-list cannot tell that apart from a storage whose
// last volume was deleted. A caller that passes none keeps the behavior it had
// before corroboration existed.
func ProveVolumeAbsent(
	ctx context.Context, client Client, node, storage, volume string, classify StorageClassifier,
	corroborators ...EmptyListingCorroborator,
) (bool, error) {
	if ctx == nil || client == nil || node == "" {
		return false, fmt.Errorf("volume absence proof requires context, client, and node")
	}
	if storage == "" || volume == "" {
		return false, fmt.Errorf("volume absence proof requires a storage name and a volume")
	}
	// The point probe reads the storage service, so a client without one
	// cannot start. That is a wiring fault rather than a cluster condition,
	// and on a delete path it has to fail closed rather than panic on the nil
	// interface. The nodes service is checked where the listing needs it, so a
	// caller whose point probe answers is not held to a surface it never uses.
	if client.Storage() == nil {
		return false, fmt.Errorf("volume absence proof requires the storage service")
	}
	exists, err := ExistsTolerant(ctx, client, node, storage, volume)
	if err == nil {
		return !exists, nil
	}
	// The point probe answers with a 404 on some backends and with CLI text on
	// the block ones, and ExistsTolerant folds both. On file storage it answers
	// with "volume_size_info ... failed - no format" for a stat that did not
	// work, whatever stopped it, so the reply cannot be read either way. The
	// info handler behind it never activates the storage, so it cannot tell a
	// missing file from an export that went away. Only a content listing can.
	if client.Nodes() == nil {
		return false, fmt.Errorf(
			"volume absence proof needs the nodes service to read a content listing (point probe: %s)", err.Error())
	}
	found, listed, observeErr := ObserveStorageVolumeContent(ctx, client, node, volume)
	if observeErr != nil {
		// Both observations failed, so the caller's warning is the only place
		// an operator will learn why. The observation error stays the wrapped
		// one, so errors.Is still matches the bounded diagnostic category the
		// managed paths test for, and the point probe's own reason rides
		// along as text rather than as a second wrapped error, so nothing
		// downstream starts classifying this outcome from the probe's HTTP
		// code. Callers log it through log.Err, which scrubs URL credentials.
		return false, fmt.Errorf("%w (point probe: %s)", observeErr, err.Error())
	}
	if found {
		return false, nil
	}
	// The storage is classified once, here, and the same answer serves both the
	// rule below and the corroborators after it. They need it for a different
	// question than the rule does: a reference or a journal record on another
	// node contradicts an empty listing only when the storage is visible from
	// more than one node, so every source has to know what kind of storage it
	// is judging.
	info, classified := classifyStorage(ctx, classify)
	if proofErr := listingProvesAbsence(storage, listed, info, classified); proofErr != nil {
		return false, proofErr
	}
	// Only an empty listing reaches corroboration. A listing that carried other
	// volumes proved the tree is really there, and the plain-dir rule above has
	// already refused the empty case on the storages where an empty answer
	// means nothing at all, so what is left here is the allow-listed or
	// is_mountpoint storage whose empty answer we are about to believe.
	if listed == 0 {
		probe := EmptyListingProbe{
			Node: node, Storage: storage, Volume: volume, Info: info, Classified: classified,
		}
		if corrErr := corroborateEmptyListing(ctx, probe, corroborators); corrErr != nil {
			return false, corrErr
		}
	}
	return true, nil
}

// classifyStorage runs the caller's classifier once, folding a caller that
// passed none into the same "not classified" answer a classifier that could not
// identify the storage gives. Nothing downstream may read the returned
// StorageInfo without checking the bool: an unclassified storage is one we know
// nothing about, and its zero value says local and not a mountpoint, neither of
// which was observed.
func classifyStorage(ctx context.Context, classify StorageClassifier) (StorageInfo, bool) {
	if classify == nil {
		return StorageInfo{}, false
	}
	info, ok := classify(ctx)
	if !ok {
		return StorageInfo{}, false
	}
	return info, true
}

// listingProvesAbsence reports whether a volid missing from a successful
// listing actually establishes that the volume is gone. It does on any storage
// PVE refuses to activate when its backing is unreachable: nfs and cifs, dir
// and btrfs carrying is_mountpoint, and the block and Ceph plugins whose own
// tooling fails rather than listing nothing.
//
// A plain dir storage with no is_mountpoint lists an empty array when its mount
// drops, and activate_storage recreates images/ under the bare path, so there an
// empty listing says nothing and only other volumes in the listing prove the
// tree is really there.
//
// It is handed the classification the caller already made rather than making
// its own, so one proof classifies the storage once and the corroborators after
// it weigh the same answer.
func listingProvesAbsence(storage string, listed int, info StorageInfo, classified bool) error {
	if !classified {
		return fmt.Errorf("storage %s could not be classified, so an empty content listing proves nothing", storage)
	}
	if info.IsMountpoint || listingFailsWhenBackingIsGone(info.Type) {
		return nil
	}
	// Everything else is a path-based plugin, or a type we do not recognize.
	// Other volumes in the listing prove the tree is really mounted; an empty
	// listing proves nothing at all.
	if listed > 0 {
		return nil
	}
	return fmt.Errorf(
		"storage %s is %s with no is_mountpoint and its content listing came back empty, "+
			"which a dropped mount produces as readily as a genuinely empty storage; set is_mountpoint "+
			"on that storage so PVE reports it offline instead of listing nothing",
		storage, describeStorageType(info.Type),
	)
}

// describeStorageType names the storage in the unproven message. A classifier
// can still answer with a storage whose type is empty, for instance a live
// index entry that carries no type, and rendering the type verbatim there
// produced "is a  storage", so an empty type is named for what it is. The
// conservative rule is unchanged either way, because an unclassified storage
// takes the same path as an unrecognized one: only other volumes in the listing
// prove the tree is really there.
func describeStorageType(storageType string) string {
	if strings.TrimSpace(storageType) == "" {
		return "an unclassified storage"
	}
	return "a " + storageType + " storage"
}

// listingFailsWhenBackingIsGone names the storage types whose own listing path
// errors rather than returning nothing when the backing is unreachable, so a
// volid missing from a successful listing is an absence. It is an allow-list on
// purpose: a plugin nobody anticipated lands on the conservative side, where we
// ask to see other volumes before believing an empty answer.
//
// dir and btrfs are left out because they are the path-based plugins this rule
// exists for, glusterfs joins them because a fuse mount can also present an
// empty directory once its backing goes away, and pbs never holds disk volumes,
// so it never reaches here.
func listingFailsWhenBackingIsGone(storageType string) bool {
	switch strings.ToLower(strings.TrimSpace(storageType)) {
	case StorageTypeNFS, StorageTypeCIFS, StorageTypeRBD, StorageTypeCephFS,
		StorageTypeLVM, StorageTypeLVMThin, StorageTypeZFSPool:
		return true
	}
	return false
}

// EmptyListingCorroborator is a second opinion on an empty content listing. The
// proof consults it only when the listing came back empty on a storage the
// allow-list or is_mountpoint would otherwise trust, so a corroborator costs
// nothing on every other path and never has to guess at what the listing meant.
//
// A corroborator answers in one of three ways. It contradicts the listing, and
// the proof fails closed naming what contradicted it. It errors, and the proof
// fails closed too, because a check that did not land cannot clear an empty
// answer. Or it has nothing to say, and the next corroborator gets its turn.
//
// The volume under proof travels in the probe because a source that still knows
// of that one volume knows nothing the proof does not already suspect: its
// absence is the question, so its own record must never count as evidence
// against the listing. A source that tracks volumes by name has to exclude it;
// the ones that count references or read a capacity figure have nothing to
// exclude and ignore that field.
type EmptyListingCorroborator interface {
	// CorroborateEmptyListing is asked about the one storage, node, and
	// volume the probe describes.
	CorroborateEmptyListing(ctx context.Context, probe EmptyListingProbe) (Corroboration, error)
}

// EmptyListingProbe is the question a corroborator is asked: which volume is
// being proven absent, from which storage, as seen from which node, and what
// kind of storage that is.
//
// Info and Classified are what keep a source from reading evidence off the
// wrong node. A shared storage shows one tree to the whole cluster, so a
// reference or a journal record anywhere in it contradicts an empty listing. A
// node-local storage shows a different tree on every node, and a dir storage
// named "local" exists on all of them, so only what the probed node itself
// holds says anything about the listing that node served.
type EmptyListingProbe struct {
	// Node is the node the listing was read from.
	Node string
	// Storage is the storage the listing covered.
	Storage string
	// Volume is the volume under proof, as a bare name or a full volid.
	Volume string
	// Info is the storage classification the proof made. It is meaningful
	// only when Classified is true.
	Info StorageInfo
	// Classified is false when the storage could not be identified. A source
	// that reads Info without checking it would take the zero value for an
	// observation, so the honest reading of a false here is the narrow one:
	// weigh only what the probed node holds, which cannot contradict a
	// listing the way evidence from elsewhere in the cluster could.
	Classified bool
}

// Corroboration is one corroborator's answer. The zero value is "nothing to
// say", which is what a corroborator returns when it has no evidence either
// way rather than when it found the storage genuinely empty: no source here
// can prove emptiness, only contradict it.
type Corroboration struct {
	// Contradicted is true when this source knows of volumes the listing
	// should have carried.
	Contradicted bool
	// Source names where the evidence came from, for the operator-facing
	// refusal. Empty falls back to the name the corroborator carries.
	Source string
	// Detail is the one clause that follows the source in the refusal, such as
	// "3 volumes on the storage are referenced by VM configs".
	Detail string
}

// The source names the refusal prints. They are the operator's whole answer to
// "how do you know?", so they name the record rather than the code that read it.
const (
	// CorroborationSourceConfigs is the cluster's own VM configs, read by the
	// holder scan the caller already paid for.
	CorroborationSourceConfigs = "cluster configs"
	// CorroborationSourceJournal is the CPI's allocation journal, which
	// records what we allocated on a storage and what we recorded deleting.
	CorroborationSourceJournal = "allocation journal"
	// CorroborationSourceStorageStatus is PVE's own status for the storage on
	// the node, which reports activity and used bytes.
	CorroborationSourceStorageStatus = "storage status"
)

// EmptyListingUsedBytesFloor is the used figure above which a storage that
// listed nothing is worth warning about. It sits well above what an empty
// dir-style storage reports for its own images/ and dump/ subdirectories, and
// well below any disk BOSH would have put there, so neither side of the
// comparison is a close call.
//
// It gates a warning and not a refusal. PVE answers the figure from a statfs of
// the filesystem behind the storage, which counts bytes no listing of this
// storage's content types would ever show, so an export shared between an
// images storage and a backup storage crosses the floor while both storages are
// telling the truth.
const EmptyListingUsedBytesFloor = 1 << 30

// emptyListingStatusTimeout bounds one attempt at the storage status read. It
// matches the other single-read probes in this package, and it exists because
// the read happens on a delete path that has already spent a point probe and a
// listing.
const emptyListingStatusTimeout = 30 * time.Second

// corroborationSourceNamer is implemented by the corroborators in this package
// so a refusal can name the source even when the answer carried no verdict, as
// an error does.
type corroborationSourceNamer interface {
	CorroborationSource() string
}

// corroborateEmptyListing consults the corroborators in order and stops at the
// first one that has something to say. Order is the caller's, and it is the
// cost order: the sources that read records already in hand come before the one
// that spends an API call, so a contradiction the cheap sources can see is
// found without touching the cluster.
func corroborateEmptyListing(
	ctx context.Context, probe EmptyListingProbe, corroborators []EmptyListingCorroborator,
) error {
	for _, corroborator := range corroborators {
		if corroborator == nil {
			continue
		}
		verdict, err := corroborator.CorroborateEmptyListing(ctx, probe)
		if err != nil {
			return fmt.Errorf(
				"storage %s listed no content and the %s check that would corroborate it did not land, "+
					"so the volume is not proven gone: %w",
				probe.Storage, corroborationSource(corroborator, verdict.Source), err)
		}
		if verdict.Contradicted {
			return fmt.Errorf(
				"storage %s listed no content but its absence is contradicted by %s: %s; the export may be "+
					"mounted from the wrong tree, so the volume is not proven gone",
				probe.Storage, corroborationSource(corroborator, verdict.Source), corroborationDetail(verdict.Detail))
		}
	}
	return nil
}

// corroborationSource picks the name the refusal prints: the one the answer
// carried, then the one the corroborator carries, and failing both a phrase
// that at least says what kind of check it was.
func corroborationSource(corroborator EmptyListingCorroborator, source string) string {
	if trimmed := strings.TrimSpace(source); trimmed != "" {
		return trimmed
	}
	if namer, ok := corroborator.(corroborationSourceNamer); ok {
		if trimmed := strings.TrimSpace(namer.CorroborationSource()); trimmed != "" {
			return trimmed
		}
	}
	return "an unnamed empty-listing check"
}

// corroborationDetail keeps the refusal readable when a corroborator
// contradicted the listing without saying what it saw.
func corroborationDetail(detail string) string {
	if trimmed := strings.TrimSpace(detail); trimmed != "" {
		return trimmed
	}
	return "it reported no detail"
}

// CorroboratorFunc adapts a function to EmptyListingCorroborator, carrying the
// source name so a caller composing its own check gets the same refusal wording
// as the built-in ones. A nil fn is a wiring fault rather than a corroborator
// with nothing to say, so it reports an error and the proof fails closed.
func CorroboratorFunc(
	source string, fn func(ctx context.Context, probe EmptyListingProbe) (Corroboration, error),
) EmptyListingCorroborator {
	return corroboratorFunc{source: source, fn: fn}
}

type corroboratorFunc struct {
	source string
	fn     func(ctx context.Context, probe EmptyListingProbe) (Corroboration, error)
}

func (c corroboratorFunc) CorroborationSource() string { return c.source }

func (c corroboratorFunc) CorroborateEmptyListing(
	ctx context.Context, probe EmptyListingProbe,
) (Corroboration, error) {
	if c.fn == nil {
		return Corroboration{Source: c.source}, fmt.Errorf("empty-listing corroborator %q has no implementation", c.source)
	}
	return c.fn(ctx, probe)
}

// ConfigReferenceCorroborator contradicts an empty listing when the cluster's
// VM configs still reference volumes on the storage. The counts come from the
// holder scan delete_disk and the attach paths already run, so this source
// costs no API call where it is available, and it is available nowhere else:
// refs is nil on every caller that never scanned, and a nil map has nothing to
// say rather than something to deny.
//
// Which references count depends on the storage. A shared storage is one tree
// the whole cluster sees, so a config on any node naming a volume on it
// contradicts a listing that showed nothing. A node-local storage is a
// different tree on every node, and the dir storage PVE calls "local" exists on
// all of them, so only the configs of guests on the probed node say anything
// about the listing that node served. An unclassified storage takes the
// node-local reading, which is the one that cannot manufacture a contradiction
// out of another node's disks.
//
// The volume under proof needs no excluding here. The scan counts what VM
// configs reference, and a volume any config still references has a holder, so
// a caller that reached an absence proof for it is not the caller these counts
// come back to.
func ConfigReferenceCorroborator(refs StorageReferenceCounts) EmptyListingCorroborator {
	return CorroboratorFunc(CorroborationSourceConfigs,
		func(_ context.Context, probe EmptyListingProbe) (Corroboration, error) {
			referenced, scope := configReferencesInScope(refs, probe)
			if referenced <= 0 {
				return Corroboration{}, nil
			}
			return Corroboration{
				Contradicted: true,
				Source:       CorroborationSourceConfigs,
				Detail: fmt.Sprintf("%s on the storage %s referenced by VM configs%s",
					pluralVolumes(referenced), isAre(referenced), scope),
			}, nil
		})
}

// configReferencesInScope picks the count the probe may read and the clause the
// refusal appends to name where those references live, which is the difference
// between "somewhere in the cluster" and "on the node we just probed".
func configReferencesInScope(refs StorageReferenceCounts, probe EmptyListingProbe) (int, string) {
	if probe.Classified && probe.Info.IsShared() {
		return refs.Anywhere(probe.Storage), ""
	}
	return refs.OnNode(probe.Storage, probe.Node), " on node " + probe.Node
}

// pluralVolumes renders a reference count as the noun phrase the refusal reads.
func pluralVolumes(n int) string {
	if n == 1 {
		return "1 volume"
	}
	return fmt.Sprintf("%d volumes", n)
}

// isAre agrees the verb with the count, so the refusal reads as a sentence.
func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// StorageStatusCorroborator contradicts an empty listing when PVE reports the
// storage inactive on the node. It is the last resort because it is the only
// corroborator that spends an API call, and an inactive storage is a storage
// that cannot have proved anything absent.
//
// The used figure it also reads is advisory. PVE answers it from a statfs of
// the whole filesystem behind the storage, while a content listing covers only
// the storage's configured content types, so one NFS export carrying a backup
// storage's dump directory alongside an images storage reports those bytes
// against every honest empty listing on the images storage. Contradicting on
// that would wedge has_disk and both orphan sweeps with no way out, so the
// figure is logged for the operator chasing a wrong export and nothing more.
//
// It has no volume to exclude: PVE reports one figure for the whole storage,
// and neither the activity flag nor the used figure is about a single volume.
func StorageStatusCorroborator(client Client) EmptyListingCorroborator {
	return CorroboratorFunc(CorroborationSourceStorageStatus,
		func(ctx context.Context, probe EmptyListingProbe) (Corroboration, error) {
			return readStorageStatusCorroboration(ctx, client, probe.Node, probe.Storage)
		})
}

// readStorageStatusCorroboration performs the one status read behind
// StorageStatusCorroborator. Every failure is returned rather than swallowed:
// the caller is about to conclude that a volume is gone, and a status read that
// did not land is not a status read that agreed.
func readStorageStatusCorroboration(ctx context.Context, client Client, node, storage string) (Corroboration, error) {
	if ctx == nil || client == nil {
		return Corroboration{}, fmt.Errorf("storage status corroboration requires context and client")
	}
	if node == "" || storage == "" {
		return Corroboration{}, fmt.Errorf("storage status corroboration requires a node and a storage name")
	}
	if client.Nodes() == nil {
		return Corroboration{}, fmt.Errorf("storage status corroboration requires the nodes service")
	}
	// The read rides the package's transient retry, the way every other single
	// read here does. It is the last thing standing between a delete path and a
	// permanent refusal, and a pvedaemon worker recycling during the one call
	// is exactly the fault that clears on the next attempt. A permanent answer
	// still comes straight back, because RetryOnTransient only re-drives
	// transport faults and pushback. Each attempt carries its own timeout, so a
	// hung read cannot spend the whole budget.
	var status *nodes.ListStorageStatusResponse
	err := RetryOnTransient(ctx, nil, "empty_listing_storage_status", 0, func() error {
		statusCtx, cancel := context.WithTimeout(ctx, emptyListingStatusTimeout)
		defer cancel()
		read, readErr := client.Nodes().ListStorageStatus(statusCtx, node, storage)
		if readErr != nil {
			return readErr
		}
		status = read
		return nil
	})
	if err != nil {
		return Corroboration{}, fmt.Errorf("read status of storage %s on node %s: %w", storage, node, err)
	}
	if status == nil {
		return Corroboration{}, fmt.Errorf("status of storage %s on node %s came back empty", storage, node)
	}
	// PVE answers these as integers or as strings depending on the endpoint and
	// the version, which is why the SDK decodes them through its wire scalars
	// and why nothing here reads a plain int.
	if status.Active == nil || !status.Active.Bool() {
		return Corroboration{
			Contradicted: true,
			Source:       CorroborationSourceStorageStatus,
			Detail:       fmt.Sprintf("storage is not active on node %s", node),
		}, nil
	}
	if status.Used == nil {
		return Corroboration{}, nil
	}
	used := status.Used.Int()
	if used < EmptyListingUsedBytesFloor {
		return Corroboration{}, nil
	}
	// Bytes in use against a listing that showed nothing is worth an operator's
	// attention and is not evidence. The two readings are of different things:
	// the figure covers the whole filesystem the storage sits on, the listing
	// covers the content types the storage is configured for, so an export
	// shared with a backup storage reports its dumps here every time.
	log.FromContext(ctx).Warn(
		"storage lists nothing but reports bytes in use; a shared export reports its whole filesystem, "+
			"so this is a hint to check the export rather than proof the listing is wrong",
		log.String("storage", storage),
		log.String("node", node),
		log.Int64("used_bytes", used),
		log.Int64("total_bytes", storageTotalBytes(status.Total)),
	)
	return Corroboration{}, nil
}

// storageTotalBytes reads the capacity the used figure sits against, so an
// operator reading the warning can tell a full storage from a nearly empty one
// without going to look. PVE omits it on some storages, and zero is how the
// warning says so.
func storageTotalBytes(total *sdkclient.PVEInt) int64 {
	if total == nil || total.Int() < 0 {
		return 0
	}
	return total.Int()
}
