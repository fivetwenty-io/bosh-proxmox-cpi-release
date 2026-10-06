package pve

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// parkerDescriptionWriteAttempts bounds the read-and-write rounds of every
// digest-guarded parker description write. A round ends early only when the
// write was refused because another writer changed the parker's config after
// our read, so each extra round costs one config read and one write.
const parkerDescriptionWriteAttempts = 3

// ErrConfigDigestStale marks a digest-guarded configuration write that a client
// wrapper refused before sending it, because the digest the write carried no
// longer matched the wrapper's own read of the VM. Nothing reached PVE, so the
// configuration is as the other writer left it. IsConfigDigestStale matches it
// alongside PVE's own refusal.
var ErrConfigDigestStale = errors.New("the configuration changed after the read the write's digest came from")

// ErrParkerDescriptionContended reports that a parker's config changed between
// our read and our write on every one of a guarded write's rounds, so nothing
// of ours was written. Each refused round wrote nothing over the other writer.
// The condition is transient: it lasts only while other requests keep writing
// to the same parker.
var ErrParkerDescriptionContended = errors.New("parker configuration kept changing under the description write")

// errParkerConfigNoDigest reports that every read of a parker's config that a
// guarded description write made came back without a digest, so the write was
// never sent. PVE's config read always carries one, so a read without it is a
// read that did not deliver the config, and a description built from it could
// erase every record on the parker. Nothing was written, and a retry reads the
// parker again.
var errParkerConfigNoDigest = errors.New("parker config read carries no digest")

// IsConfigDigestStale reports whether err says a digest-guarded configuration
// write was refused because the configuration changed after the read its
// digest came from. That is PVE's own answer (IsConfigDigestRefusal), or a
// client wrapper that refused the write before sending it (ErrConfigDigestStale).
// Either way nothing was written, and the writer can read again and retry.
func IsConfigDigestStale(err error) bool {
	return IsConfigDigestRefusal(err) || errors.Is(err, ErrConfigDigestStale)
}

// parkerDescriptionEdit builds the description a guarded write sends from one
// read of the parker's config. It returns write=false when that read needs no
// write, and the guarded write then stops without writing and without an error.
// An error stops the guarded write and comes back unchanged.
type parkerDescriptionEdit func(vmCfg map[string]any) (desc string, write bool, err error)

// parkerDescriptionWrite describes one guarded read-modify-write of a parker's
// description.
type parkerDescriptionWrite struct {
	node string
	vmid int
	// what names the write at the front of its errors, such as
	// "parker provenance".
	what string
	edit parkerDescriptionEdit
}

// writeParkerDescriptionGuarded runs w's read-modify-write with the digest of
// each read on its write, so PVE refuses the write when the parker's config
// changed in between instead of overwriting what another writer just wrote.
// After a refusal it reads again and lets w.edit apply its change to the fresh
// read, so another holder's entry survives and only the entries this write owns
// change.
//
// A read that carries no digest is never written from, because a write built
// on it could not be guarded, and such a read is most likely one that did not
// deliver the config at all, so the description built from it would erase
// every other record and the operator's text. The round reads again instead.
//
// The rounds are bounded by parkerDescriptionWriteAttempts. When every round is
// refused it returns ErrParkerDescriptionContended as a retriable error and has
// written nothing. When the last round's read still carried no digest it
// returns errParkerConfigNoDigest as a retriable error and has written nothing.
// A failed read, a failed edit, and any other write failure end the write at
// once, because another round would only meet the same failure.
// A client without a nodes service writes nothing and reports no error, which
// matches every other sentinel writer in this package.
func writeParkerDescriptionGuarded(ctx context.Context, c Client, w parkerDescriptionWrite) error {
	if c == nil || w.edit == nil || w.node == "" || w.vmid <= 0 {
		return cpierrors.Cloud("%s: guarded description write needs a client, an edit, a node, and a parker VMID", w.what)
	}
	vmidStr := strconv.Itoa(w.vmid)
	lastReadHadNoDigest := false
	for attempt := 1; attempt <= parkerDescriptionWriteAttempts; attempt++ {
		vmCfg, err := c.QEMU().Config(ctx, w.node, w.vmid)
		if err != nil {
			return cpierrors.Wrap(WrapConfigReadError(err),
				fmt.Sprintf("%s: config fetch for parker vmid %d", w.what, w.vmid))
		}
		digest, ok := ConfigString(vmCfg, "digest")
		lastReadHadNoDigest = !ok || digest == ""
		if lastReadHadNoDigest {
			// Nothing is built from this read, so not even an edit that would
			// write nothing runs on it.
			continue
		}
		desc, write, editErr := w.edit(vmCfg)
		if editErr != nil {
			return editErr
		}
		if !write {
			return nil
		}

		params := &sdknodes.UpdateQemuConfigParams{Description: &desc, Digest: &digest}
		nodesSvc := c.Nodes()
		if nodesSvc == nil {
			// No nodes service available (e.g. test stub without injection). Skip silently.
			return nil
		}
		updateErr := nodesSvc.UpdateQemuConfig(ctx, w.node, vmidStr, params)
		if updateErr == nil {
			return nil
		}
		if !IsConfigDigestStale(updateErr) {
			return cpierrors.Wrap(WrapMutationError(updateErr),
				fmt.Sprintf("%s: UpdateQemuConfig for parker vmid %d", w.what, w.vmid))
		}
		// The config changed after our read, so nothing was written. Read again.
	}
	if lastReadHadNoDigest {
		return cpierrors.WrapAs(errParkerConfigNoDigest, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: parker vmid %d on node %s answered its last config read without a digest, so nothing was written; retry",
				w.what, w.vmid, w.node))
	}
	return cpierrors.WrapAs(ErrParkerDescriptionContended, cpierrors.TypeRetriableCloud,
		fmt.Sprintf("%s: parker vmid %d on node %s changed between our read and our write on each of %d attempts, so nothing was written; retry",
			w.what, w.vmid, w.node, parkerDescriptionWriteAttempts))
}
