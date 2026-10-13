package handlers

import (
	"context"
	"slices"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// resolveCreateStamp fills parsed.clusterStamp and parsed.cidToken once for
// the whole create_vm call, ahead of placement, so every candidate shape
// carries the same stamp tags and the CID the call returns names them.
//
// The cluster stamp comes from pve.ReadClusterStamp, starting at the
// configured node. A failed read does not fail the create. The VM gets the
// unknown stamp instead, which gives it no cluster protection, and the
// warning names the reason, because the failure can be permanent when a
// node's certificate data is malformed. A token the system random source
// cannot supply does fail the create, because a CID without a real token
// would match any VM that later takes the VMID.
//
// The tags are written in both vm_cid_format modes, so a later switch to
// stamped CIDs finds every VM already tagged.
func resolveCreateStamp(ctx context.Context, deps Deps, parsed *createVMParsedArgs, logger *log.Logger) error {
	if parsed == nil {
		return cpierrors.Cloud("create_vm: no parsed arguments to stamp")
	}
	token, err := vmcid.NewToken()
	if err != nil {
		return cpierrors.Wrap(err, "create_vm: generate the VM CID token")
	}
	node := ""
	if deps.Config != nil {
		node = deps.Config.Node
	}
	var stamp string
	if deps.PVE == nil {
		err = cpierrors.Retriable("no PVE client is available to read the cluster identity")
	} else {
		stamp, err = pve.ReadClusterStamp(ctx, deps.PVE, node)
	}
	if err != nil {
		logger.Warn("create_vm: the cluster identity could not be read, so the VM is stamped with the unknown cluster "+
			vmcid.UnknownCluster+" and its CID gets no protection against naming another cluster",
			log.Err(err))
		stamp = vmcid.UnknownCluster
	}
	parsed.clusterStamp = stamp
	parsed.cidToken = token
	return nil
}

// stampTags returns the cluster and token tags for parsed, or nil when
// resolveCreateStamp has not run, which leaves the VM untagged and its CID
// bare. A caller that builds a shape outside create_vm, such as a test, gets
// today's tags.
func stampTags(parsed *createVMParsedArgs) []string {
	if parsed == nil || parsed.clusterStamp == "" || parsed.cidToken == "" {
		return nil
	}
	return []string{clusterStampTagPrefix + parsed.clusterStamp, cidStampTagPrefix + parsed.cidToken}
}

// requireStampTags fails a shape whose merged tag list lost a stamp tag to the
// tag-length cap. Every VM CID create_vm returns is built from these tags, so a
// VM without them would get a CID that its own checks refuse.
func requireStampTags(parsed *createVMParsedArgs, initialTags string) error {
	kept := parseTagsField(initialTags)
	for _, tag := range stampTags(parsed) {
		if !slices.Contains(kept, tag) {
			return cpierrors.Cloud("create_vm: the tag list exceeds the %d-byte cap and would drop the %s tag, which "+
				"ties the VM to its CID. Shorten the director_uuid or the cloud_properties tags", maxTagLength, tag)
		}
	}
	return nil
}

// directorVMCID is the VM CID create_vm returns for the VM it built or
// resumed. Under vm_cid_format: bare it is the VMID, as in earlier releases.
// Otherwise it is the stamped CID rebuilt from the VM's own cluster and token
// tags, so a resumed create returns the CID its first attempt would have. A VM
// whose tags do not name exactly one valid cluster and one valid token, which
// is every VM an earlier release created, gets the bare CID. That CID is safe
// and only unprotected.
func directorVMCID(cfg *config.CPIConfig, vmid int, tagsRaw string) string {
	if cfg.VMCIDFormatValue() == config.VMCIDFormatBare {
		return vmcid.PathOf(vmid)
	}
	if cid, ok := stampedCIDFromTags(vmid, tagsRaw); ok {
		return cid.String()
	}
	return vmcid.PathOf(vmid)
}

// stampedCIDFromTags rebuilds a VM's stamped CID from its tags. ok is false
// when the tags carry no cluster or no token tag, more than one distinct value
// for either, or a value that is not a valid stamp.
func stampedCIDFromTags(vmid int, tagsRaw string) (vmcid.VMCID, bool) {
	tags := lowerTags(parseTagsField(tagsRaw))
	cluster, okCluster := soleTagValue(tags, clusterStampTagPrefix)
	token, okToken := soleTagValue(tags, cidStampTagPrefix)
	if !okCluster || !okToken {
		return vmcid.VMCID{}, false
	}
	cid, err := vmcid.Stamp(vmid, cluster, token)
	if err != nil {
		return vmcid.VMCID{}, false
	}
	return cid, true
}

// soleTagValue returns the one distinct value tags carry under prefix. ok is
// false when there is none or more than one.
func soleTagValue(tags []string, prefix string) (string, bool) {
	values := tagValues(tags, prefix)
	if len(values) == 0 {
		return "", false
	}
	for _, v := range values[1:] {
		if v != values[0] {
			return "", false
		}
	}
	return values[0], true
}
