// create_vm_shape.go assembles the per-node createVMShape (storage,
// cpu/mem, cpu type, balloon, hotplug/NUMA, initial name, pool, and clone
// create-params) used by the create_vm orchestration.
// Split out of create_vm.go (mechanical move, no behavior change).
package handlers

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// buildVMShapeForNode constructs a createVMShape for a pre-resolved node.
// All node selection (resolveTargetNode / resolveTargetNodeWithFallbacks) must
// happen before calling this helper; the resulting node string is passed in.
// vmStorageType is populated via a best-effort cluster storage list lookup;
// on failure (PVE unavailable, ClusterStorage not wired) the field is left ""
// so IsLinkedCloneSupported treats it as linked-capable (permissive default).
func buildVMShapeForNode(ctx context.Context, deps Deps, parsed *createVMParsedArgs, node string) (*createVMShape, error) {
	cp := parsed.cloudProps

	rangeStart, maxAttempts := resolveVMIDAllocParams(deps.Config)
	// Build a tier-resolver closure so resolveVMShapeStorage can call
	// resolveStorageTier without its signature carrying ctx/Deps directly.
	// The closure is only invoked when cloud_properties.storage_tier is set
	// in the resolver layers; nil ClusterStorage falls through to config fallback.
	var tierFnForVM vmStorageTierFn
	if deps.PVE != nil && deps.PVE.ClusterStorage() != nil {
		lister := deps.PVE.ClusterStorage()
		cfg := deps.Config
		tierFnForVM = func(tier string) (string, error) {
			// VM root disk does not apply the encrypted filter (§7.49 applies to
			// persistent and ephemeral disks only). Pass encrypted=false.
			return resolveStorageTier(ctx, lister, cfg, tier, false)
		}
	}
	vmStorage, vmDiskFormat, rootDiskGiB, err := resolveVMShapeStorage(deps.Config, parsed, tierFnForVM)
	if err != nil {
		return nil, err
	}
	cores, sockets, memMiB := resolveVMShapeCPUMem(cp)
	hotplug, numaEnabled, err := resolveVMShapeHotplugNUMAWithError(deps.Config, cp, parsed.cloudPropsMap)
	if err != nil {
		return nil, err
	}

	// "bosh-cpi" is the fixed ownership marker stamped on every CPI-created VM.
	// It lets operators filter PVE UI / scripts to CPI-managed guests only and
	// is preserved across set_vm_metadata because it is not in
	// reservedBoshTagPrefixes. Operator-supplied tags are appended after.
	// The BOSH-managed director/deployment/job triple is added later by
	// set_vm_metadata.
	baseRetainTags := buildCustomTags(cp.Tags)
	if cp.RetainEphemeralOnDelete != nil && *cp.RetainEphemeralOnDelete {
		baseRetainTags = append(baseRetainTags, tagRetainEphemeral)
	}
	// Advertised-route provenance: one advrt-<vnet>-<hash8> tag per route so
	// delete_vm can remove the matching SDN subnets (refcounted across VMs).
	// Baked into the initial tag set — zero extra API calls, and the tags die
	// with the VM on create rollback.
	advrtTags := advertisedRouteTags(cp.AdvertisedRoutes)
	baseRetainTags = append(baseRetainTags, advrtTags...)
	initialTags := mergeTagList([]string{ownershipTag}, baseRetainTags, maxTagLength)
	for _, tag := range advrtTags {
		if !strings.Contains(initialTags, tag) {
			deps.Log(ctx).Warn("create_vm: advertised-route provenance tag dropped by tag-length cap — "+
				"that route loses automatic cleanup on delete_vm",
				log.String("tag", tag))
		}
	}
	initialName := resolveVMShapeInitialName(deps.Config, parsed)

	// Best-effort: populate vmStorageType for the clone-mode decision in
	// cloneFromTemplate. A lookup error leaves the field "" which
	// IsLinkedCloneSupported treats as linked-capable (permissive).
	vmStorageType := lookupVMStorageType(ctx, deps, vmStorage)

	// rootDiskKeyVal is the PVE VM config key the root disk lands on: virtio0
	// (default) or scsi0 (pve.root_disk_bus=scsi).
	rootDiskKeyVal := rootDiskKey(deps.Config)

	// Resolve per-disk performance options for the root disk.
	// newLayeredResolver is cheap (no I/O); building a dedicated resolver here
	// avoids threading it through resolveVMShapeStorage's signature.
	// On error (invalid cloud_property value) we propagate a CloudError
	// immediately before any VM is created.
	perfR, perfRErr := newLayeredResolver(parsed.cloudPropsMap, deps.Config)
	if perfRErr != nil {
		return nil, perfRErr
	}
	rawPerfOpts, perfOptsErr := resolveDiskPerfOptions(perfR, deps.Config, vmStorageType, vmDiskFormat)
	if perfOptsErr != nil {
		return nil, perfOptsErr
	}
	// virtio0 is a virtio-blk device: the "ssd" flag is invalid on that bus, so
	// filterDiskPerfForBus("virtio") removes it while keeping iothread/cache/etc.
	// A scsi0 root disk (pve.root_disk_bus=scsi) lives on the same virtio-scsi
	// controller persistent disks use, so filterDiskPerfForBus("scsi") keeps ssd
	// too — composing correctly with the discard/ssd TRIM-capability auto-
	// resolution in resolveDiskPerfOptions.
	rootDiskPerfOpts := filterDiskPerfForBus(rawPerfOpts, rootDiskBusName(deps.Config))

	scsihwVal := "virtio-scsi-pci"
	if resolveVirtioSCSISingle(perfR, deps.Config) {
		scsihwVal = "virtio-scsi-single"
	}

	cpuTypeVal := resolveVMShapeCPUType(perfR, deps.Config)

	balloonStr, balloonErr := resolveVMShapeBalloon(perfR, deps.Config)
	if balloonErr != nil {
		return nil, balloonErr
	}
	var balloonMiB *int
	if balloonStr != "" {
		n, convErr := strconv.Atoi(balloonStr)
		if convErr != nil {
			return nil, cpierrors.Cloud(
				"create_vm: balloon value %q is not an integer (MiB)", balloonStr,
			)
		}
		if n > memMiB {
			return nil, cpierrors.Cloud(
				"create_vm: balloon %d MiB exceeds VM memory %d MiB — set cloud_properties.balloon (or pve.balloon) to at most the VM's memory",
				n, memMiB,
			)
		}
		balloonMiB = &n
	}

	ephemeralDiskGiB, ephemeralStorage, err := resolveEphemeralShape(ctx, deps, cp, parsed.cloudPropsMap)
	if err != nil {
		return nil, err
	}
	if err := enforceEphemeralMinSize(deps.Config, deps.Log(ctx), ephemeralDiskGiB, memMiB); err != nil {
		return nil, err
	}

	// Resolve the PVE resource pool this VM is assigned to (plan §0/D-04:
	// call > vm_type > vm_pool_template > global vm_pool). newPoolResolver
	// deliberately excludes the disk_type layer perfR (above) includes — a
	// disk_type profile's cloud_properties.pool must never outrank vm_type.
	poolR, poolRErr := newPoolResolver(parsed.cloudPropsMap, deps.Config)
	if poolRErr != nil {
		return nil, poolRErr
	}
	resolvedPool, resolvedPoolLayer, poolErr := resolvePoolName(deps.Config, poolR, parsed.env)
	if poolErr != nil {
		return nil, poolErr
	}
	var vmPoolComment, poolDirector, poolDeployment, poolInstanceGroup string
	if resolvedPool != "" {
		// poolTemplateTokensFromEnv is renderPoolTemplate's own derivation,
		// so the director extracted here (for the provenance comment) and the
		// three tokens persisted in the bosh_pool sentinel are consistent
		// with whatever produced a template-rendered pool name.
		poolDirector, poolDeployment, poolInstanceGroup = poolTemplateTokensFromEnv(deps.Config, parsed.env)
		vmPoolComment = pve.PoolProvenance(poolDirector)
	}

	return &createVMShape{
		node:              node,
		vmStorage:         vmStorage,
		vmStorageType:     vmStorageType,
		vmDiskFormat:      vmDiskFormat,
		rootDiskGiB:       rootDiskGiB,
		cores:             cores,
		sockets:           sockets,
		memMiB:            memMiB,
		hotplug:           hotplug,
		numaEnabled:       numaEnabled,
		initialTags:       initialTags,
		rangeStart:        rangeStart,
		maxAttempts:       maxAttempts,
		initialName:       initialName,
		cloudPropsMap:     parsed.cloudPropsMap,
		rootDiskPerfOpts:  rootDiskPerfOpts,
		rootDiskKey:       rootDiskKeyVal,
		scsihw:            scsihwVal,
		ephemeralDiskGiB:  ephemeralDiskGiB,
		ephemeralStorage:  ephemeralStorage,
		cpuType:           cpuTypeVal,
		balloonMiB:        balloonMiB,
		vmPool:            resolvedPool,
		vmPoolComment:     vmPoolComment,
		vmPoolLayer:       resolvedPoolLayer,
		vmPoolDirector:    poolDirector,
		vmPoolDeployment:  poolDeployment,
		vmPoolInstanceGrp: poolInstanceGroup,
	}, nil
}

// vmStorageTierFn resolves a named storage tier to a concrete pool name.
// Passed as an optional parameter to resolveVMShapeStorage so the function
// can stay testable in the internal package without requiring ctx or Deps.
// Production callers supply a closure over ctx + deps; internal unit tests
// omit the parameter entirely (nil = tier resolution skipped).
type vmStorageTierFn func(tier string) (string, error)

// resolveVMShapeStorage returns the target VM storage, disk format, root disk
// size in GiB, and an error. The resolver checks for storage_pool and
// vm_disk_format through the layered resolver (call cloud_properties →
// disk_type profile → vm_type profile), then falls back to existing config /
// struct-field / default logic. Returns a CloudError if the vm_type or disk_type
// selector names an unknown profile. Behavior is byte-identical to the
// pre-resolver path when no profiles or storage_pool keys are present.
//
// The optional tierFn parameter enables storage_tier resolution. When nil (the
// default for internal tests and callers that do not need tier resolution),
// storage_tier in cloud_properties is silently ignored and the existing
// fallback chain applies: config.VMStorage → stemcell storage.
func resolveVMShapeStorage(cfg *config.CPIConfig, parsed *createVMParsedArgs, tierFn ...vmStorageTierFn) (vmStorage, vmDiskFormat string, rootDiskGiB int, retErr error) {
	cp := parsed.cloudProps

	// Build a layered resolver from the raw cloud_properties map. This resolves
	// vm_type / disk_type selectors and sets up precedence-ordered layers.
	// A nil cloudPropsMap (e.g. old callers / unit tests) is treated as empty.
	r, err := newLayeredResolver(parsed.cloudPropsMap, cfg)
	if err != nil {
		return "", "", 0, err
	}

	// Extract the tier resolver (nil when not provided).
	var resolveTier vmStorageTierFn
	if len(tierFn) > 0 {
		resolveTier = tierFn[0]
	}

	// Storage pool resolution: explicit pool → storage_tier (if tierFn wired) → config → stemcell fallback.
	if pool, ok := r.String("storage_pool"); ok {
		vmStorage = pool
	} else if tier, hasTier := r.String("storage_tier"); hasTier && resolveTier != nil {
		resolved, tierErr := resolveTier(tier)
		if tierErr != nil {
			return "", "", 0, tierErr
		}
		vmStorage = resolved
	} else {
		vmStorage = cfg.VMStorage
		if vmStorage == "" {
			vmStorage = parsed.stemcellStorage
		}
	}

	// Disk format: resolver wins (handles both "vm_disk_format" key in call
	// layer and profile layers) → struct field from JSON unmarshal → qcow2.
	// The struct field cp.VMDiskFormat is already populated from args[2] by
	// the standard unmarshal in parseCreateVMArgs, so we only consult it when
	// the resolver finds nothing in any layer.
	if df, ok := r.String("vm_disk_format", "disk_format"); ok {
		vmDiskFormat = df
	} else if cp.VMDiskFormat != "" {
		vmDiskFormat = cp.VMDiskFormat
	} else {
		vmDiskFormat = diskFormatQCOW2
	}

	rootDiskGiB = defaultStemcellDiskGiB
	// root_disk_size (MiB) takes precedence; fall back to disk (MiB, legacy).
	requestedMiB := 0
	if rsz, ok := r.Int("root_disk_size"); ok && rsz > 0 {
		requestedMiB = rsz
	} else if cp.RootDiskSize > 0 {
		requestedMiB = cp.RootDiskSize
	}
	if requestedMiB == 0 {
		requestedMiB = cp.Disk // may be 0 — handled below
	}
	if requestedMiB > 0 {
		requestedGiB := (requestedMiB + 1023) / 1024
		if requestedGiB > rootDiskGiB {
			rootDiskGiB = requestedGiB
		}
	}
	return vmStorage, vmDiskFormat, rootDiskGiB, nil
}

// resolveVMShapeCPUMem returns cores, sockets, and memory (MiB) honoring two
// cloud_properties conventions: vSphere-style (cpu = total vCPU count) and
// PVE-native (cores/sockets explicit). Explicit cores/sockets win when present;
// otherwise cp.CPU becomes cores with a single socket. Defaults are 2 vCPUs
// (PVE guidance: never single-thread a guest, however light the workload) and
// 512 MiB. An explicit cores/cpu value of 1 is honored as given.
func resolveVMShapeCPUMem(cp createVMCloudProps) (cores, sockets, memMiB int) {
	cores = cp.Cores
	if cores <= 0 && cp.CPU > 0 {
		cores = cp.CPU
	}
	if cores <= 0 {
		cores = 2
	}
	sockets = cp.Sockets
	if sockets <= 0 {
		sockets = 1
	}
	memMiB = cp.Memory
	if memMiB <= 0 {
		memMiB = cp.RAM
	}
	if memMiB <= 0 {
		memMiB = 512
	}
	return cores, sockets, memMiB
}

// resolveVMShapeCPUType resolves the emulated CPU type/model to write on the
// new VM's PVE "cpu" config key. Precedence (highest wins):
//
//  1. cloud_properties.cpu_type — resolved through the layered resolver, so a
//     per-call value wins over a disk_type profile value, which wins over a
//     vm_type profile value (the resolver's normal precedence order).
//  2. config.CPUTypeValue() — the pve.cpu_type global value, which
//     ApplyDefaults fills with config.DefaultCPUType ("host") when the
//     operator leaves it unset.
//
// At either layer the config.CPUTypePVEDefault sentinel ("pve-default")
// resolves to "": no "cpu" key is written and PVE falls back to its own API
// default (kvm64). This is the escape hatch back to pre-default behavior.
//
// cloud_properties.pve_config.cpu is a distinct, later mechanism (applied via
// applyPVEConfigPassthrough after this value is already written) and is not
// consulted here — it always wins as the final write when set, by virtue of
// running last in the create_vm sequence, not by any precedence logic in this
// function.
func resolveVMShapeCPUType(r *layeredResolver, cfg *config.CPIConfig) string {
	if v, found := r.String("cpu_type"); found {
		if strings.TrimSpace(v) == config.CPUTypePVEDefault {
			return ""
		}
		return v
	}
	return cfg.CPUTypeValue()
}

// applyCloneBalloon applies the resolved balloon value to a clone's resource
// UpdateQemuConfig params; the default resolution is 0 — ballooning disabled —
// matching the import path. The "pve-default" sentinel (nil) must actively
// DELETE the key rather than write nothing: the stemcell template carries
// balloon=0 (create_stemcell bakes it in, so hand-made clones inherit
// ballooning-off) and PVE's clone copies the full source config, so an
// untouched clone would inherit balloon=0 — silently identical to the
// disabled default instead of PVE's own device-enabled default the sentinel
// promises. Deleting the inherited key restores true PVE behavior (device
// enabled, balloon = memory). This differs from the cpu sentinel only because
// templates carry no explicit cpu key — there is nothing to clear there.
func applyCloneBalloon(resourceParams *sdknodes.UpdateQemuConfigParams, balloonMiB *int) {
	if balloonMiB != nil {
		balloonVal := int64(*balloonMiB)
		resourceParams.Balloon = &balloonVal
		return
	}
	del := pveConfigKeyBalloon
	if resourceParams.Delete != nil && *resourceParams.Delete != "" {
		del = *resourceParams.Delete + "," + pveConfigKeyBalloon
	}
	resourceParams.Delete = &del
}

// resolveVMShapeBalloon resolves the PVE "balloon" value to write on the new
// VM. Precedence (highest wins):
//
//  1. cloud_properties.balloon — resolved through the layered resolver
//     (call > disk_type profile > vm_type profile). Both JSON-number and
//     string forms are accepted; the config.BalloonPVEDefault sentinel
//     ("pve-default") resolves to "".
//  2. config.BalloonValue() — the pve.balloon global value, which defaults
//     to "0" (balloon device disabled) even on never-defaulted configs.
//
// The returned string is "" (write no balloon key; PVE keeps its own default
// of device-enabled with balloon = memory) or a non-negative decimal MiB
// value ("0" disables the device). A present but non-numeric, non-sentinel
// value is a fail-fast error — surfaced before any PVE API call.
//
// The resolver walks raw layers itself rather than using r.String/r.Int:
// either single-typed accessor would let a value of the other type in a
// higher-precedence layer be shadowed by a lower layer.
func resolveVMShapeBalloon(r *layeredResolver, cfg *config.CPIConfig) (string, error) {
	for _, layer := range r.layers {
		v, present := layer[pveConfigKeyBalloon]
		if !present {
			continue
		}
		if s, isStr := v.(string); isStr {
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				continue
			}
			if trimmed == config.BalloonPVEDefault {
				return "", nil
			}
			if n, err := strconv.Atoi(trimmed); err == nil && n >= 0 {
				return strconv.Itoa(n), nil
			}
			return "", cpierrors.Cloud(
				"create_vm: cloud_properties.balloon must be a non-negative integer (MiB) or %q, got %q",
				config.BalloonPVEDefault, s,
			)
		}
		// Reject fractional JSON numbers explicitly — coerceInt would
		// silently truncate them, while the string branch above rejects the
		// same value; both forms must behave identically.
		if f, isFloat := v.(float64); isFloat && f != math.Trunc(f) {
			return "", cpierrors.Cloud(
				"create_vm: cloud_properties.balloon must be a whole number of MiB, got %v", v,
			)
		}
		if n, ok := coerceInt(v); ok && n >= 0 {
			return strconv.Itoa(n), nil
		}
		return "", cpierrors.Cloud(
			"create_vm: cloud_properties.balloon must be a non-negative integer (MiB) or %q, got %v",
			config.BalloonPVEDefault, v,
		)
	}
	return cfg.BalloonValue(), nil
}

// resolveVMShapeHotplugNUMAWithError resolves hotplug + numa using
// cloud_properties → vm_type/disk_type profile → config → built-in default.
// Memory hotplug needs both numa=1 and "memory" in hotplug at create time;
// operators can override per-vm_type for stemcells that misbehave on hot-add.
// It returns a CloudError when an unknown vm_type or disk_type selector is
// present in cpMap.
//
// Hotplug precedence (pointer semantics preserved):
//  1. cp.Hotplug != nil → use *cp.Hotplug (includes explicit "" to disable)
//  2. profile layer via r.String("hotplug") (disk_type then vm_type)
//  3. config.HotplugValue()
//
// After the base string is resolved, token-level overrides are applied:
//   - cp.CPUHotplug != nil → mergeHotplugToken(hotplug, "cpu", *cp.CPUHotplug)
//   - cp.MemoryHotplug != nil → mergeHotplugToken(hotplug, "memory", *cp.MemoryHotplug)
//
// NUMA precedence:
//  1. cp.NUMA != nil → use *cp.NUMA (includes explicit false)
//  2. profile layer via r.Bool("numa") (explicit false honored)
//  3. config.NUMAValue()
//
// Memory hotplug override: when cp.MemoryHotplug=true the resolved numaEnabled
// is forced true regardless of cp.NUMA or profile settings — PVE requires
// numa=1 for memory hotplug to allocate DIMM slots at create time.
func resolveVMShapeHotplugNUMAWithError(cfg *config.CPIConfig, cp createVMCloudProps, cpMap map[string]any) (hotplug string, numaEnabled bool, err error) {
	r, err := newLayeredResolver(cpMap, cfg)
	if err != nil {
		return "", false, err
	}

	// Hotplug base: call struct pointer wins (includes explicit "").
	// The call layer IS already in r (cpMap layer 0), but cp.Hotplug is a typed
	// struct pointer — using r.String would drop an explicit "" (empty is skipped
	// by r.String). Keep the struct-pointer check as the authoritative call gate.
	hotplug = cfg.HotplugValue()
	if cp.Hotplug != nil {
		hotplug = *cp.Hotplug
	} else if v, ok := r.String("hotplug"); ok {
		// Profiles only: the call layer's "hotplug" key (if any) was already
		// covered by cp.Hotplug above — this branch reads disk_type/vm_type layers.
		// r.String skips empty strings, so a profile "" is also treated as absent
		// (consistent with explicit-value semantics; only cp.Hotplug carries the
		// disable-via-empty-string meaning).
		hotplug = v
	}

	// Token-level overrides: applied after the base string is resolved so they
	// compose with profile/config values rather than replacing them wholesale.
	if cp.CPUHotplug != nil {
		hotplug = mergeHotplugToken(hotplug, "cpu", *cp.CPUHotplug)
	}
	if cp.MemoryHotplug != nil {
		hotplug = mergeHotplugToken(hotplug, "memory", *cp.MemoryHotplug)
	}

	// NUMA: call struct pointer wins (includes explicit false).
	numaEnabled = cfg.NUMAValue()
	if cp.NUMA != nil {
		numaEnabled = *cp.NUMA
	} else if b, ok := r.Bool("numa"); ok {
		numaEnabled = b
	}

	// memory_hotplug=true forces NUMA regardless of cp.NUMA or profile value.
	// PVE allocates DIMM slots only when numa=1 at VM creation time; disabling
	// NUMA would silently break memory hotplug at runtime.
	if cp.MemoryHotplug != nil && *cp.MemoryHotplug {
		numaEnabled = true
	}

	return hotplug, numaEnabled, nil
}

// resolveVMShapeInitialName composes the initial PVE VM name from env.bosh
// fields + Config so the PVE UI shows deployment + instance-group immediately
// on come-online instead of the placeholder "vm-<vmid>". Director-mode deploys
// carry director + deployment + job in env.bosh.group; `bosh create-env` paths
// have no deployment, so Config.CreateEnvDeployment (default "create-env")
// fills that segment. set_vm_metadata later refines this to
// "<prefix>-<deployment>-<job>-<index>" once the index is known.
func resolveVMShapeInitialName(cfg *config.CPIConfig, parsed *createVMParsedArgs) string {
	initialJobName := extractJobNameFromEnv(parsed.env)
	initialDeployment := extractDeploymentFromEnv(parsed.env, initialJobName)
	if initialDeployment == "" {
		initialDeployment = cfg.CreateEnvDeployment
	}
	if initialJobName == "" {
		// create-env path: env has no group/groups. Fall back to the BOSH
		// instance-group baked into cloud_provider.template.name when it is
		// detectable from env.bosh.instance.name; otherwise leave blank and
		// let the "vm-<vmid>" placeholder stand.
		initialJobName = extractInstanceNameFromEnv(parsed.env)
	}
	return composeVMName(cfg.VMPrefix, initialDeployment, initialJobName, "")
}

// applyOptionalCreateParams adds the import-path createParams keys that are
// only emitted when their resolved shape value is non-default: numa, sockets
// (only when > 1, matching the historic single-socket-is-implicit default),
// initial tags, cpu (cloud_properties.cpu_type / pve.cpu_type — defaulted to
// "host" by ApplyDefaults; empty only via the "pve-default" sentinel,
// which means PVE keeps its own kvm64 default), and
// pool (pve.vm_pool — absent means no pool assignment, byte-identical to
// every release before that property existed).
// Extracted from attemptCreateVM to keep that function's cognitive complexity
// under the project threshold.
func applyOptionalCreateParams(createParams map[string]any, shape *createVMShape) {
	if shape.numaEnabled {
		createParams["numa"] = 1
	}
	if shape.sockets > 1 {
		createParams["sockets"] = shape.sockets
	}
	if shape.initialTags != "" {
		createParams[jsonKeyTags] = shape.initialTags
	}
	if shape.cpuType != "" {
		createParams[pveConfigKeyCPU] = shape.cpuType
	}
	// Balloon: written on every VM unless the "pve-default" sentinel resolved
	// to nil (write nothing; PVE keeps its own device-enabled default). The
	// default resolution is 0 — ballooning disabled — because BOSH sizes VMs
	// deterministically from the manifest.
	if shape.balloonMiB != nil {
		createParams[pveConfigKeyBalloon] = *shape.balloonMiB
	}
	if shape.vmPool != "" {
		createParams["pool"] = shape.vmPool
	}
}

// ensureResolvedPool creates shape.vmPool (create-if-missing, tolerating a
// concurrent/prior creation) before either create path assigns the VM to it.
// No-ops — no PVE call at all — when shape.vmPool == "" (every resolver layer
// resolved empty, byte-identical to pre-feature behavior). shape.vmPoolComment
// is only ever non-empty when shape.vmPool is also non-empty (both set
// together in buildVMShapeForNode), so it needs no separate emptiness check.
func ensureResolvedPool(ctx context.Context, deps Deps, shape *createVMShape, logger *log.Logger) error {
	if shape.vmPool == "" {
		return nil
	}
	if err := pve.EnsurePoolExists(ctx, deps.PVE, shape.vmPool, shape.vmPoolComment); err != nil {
		// A permission-denied here is a reduced-ACL cluster whose token
		// cannot create pools (no Pool.Allocate at /pool). That is a
		// permanent configuration condition with a config-side fix, so name
		// it instead of surfacing a bare 403.
		if pve.IsPoolPermissionDenied(err) {
			return cpierrors.Cloud(
				"create_vm: PVE token may not create resource pool %q (Pool.Allocate denied); grant "+
					"Pool.Allocate at /pool (per-deployment pools are created on demand), or set "+
					"pve.vm_pool_template: \"\" to keep the single static pool on clusters whose token "+
					"cannot create pools: %s",
				shape.vmPool, err.Error(),
			)
		}
		return err
	}
	logger.Debug("create_vm: resolved pool ensured",
		log.String("pool", shape.vmPool),
	)
	return nil
}

// persistPoolMembership records the VM's create-time pool resolution (name,
// winning layer, and template tokens) in the bosh_pool description sentinel.
// Called after the VM exists on both create paths — on the clone path this
// must run after the post-clone description clear (the clone strips the
// inherited template identity wholesale), which happens inside the allocate
// attempt, so any post-allocation call site is safe. Best-effort: a failed
// write is logged inside pve.UpdatePoolMembership and degrades that VM to
// the legacy-adoption reconciliation rules, never failing create_vm.
func persistPoolMembership(ctx context.Context, deps Deps, logger *log.Logger, shape *createVMShape, vmid int) {
	if shape.vmPool == "" {
		return
	}
	pve.UpdatePoolMembership(ctx, deps.PVE, logger, shape.node, vmid, &pve.PoolMembership{
		Name:          shape.vmPool,
		Layer:         shape.vmPoolLayer,
		Director:      shape.vmPoolDirector,
		Deployment:    shape.vmPoolDeployment,
		InstanceGroup: shape.vmPoolInstanceGrp,
	})
}

// composeVMName builds the PVE VM name from prefix + deployment + job +
// optional index. Empty segments are dropped, so a metadata payload missing
// the deployment still yields "<prefix>-<job>-<index>" rather than a
// double-dash. Returns "" when no segment is populated; the caller then
// falls back to the "vm-<vmid>" placeholder.
func composeVMName(prefix, deployment, job, index string) string {
	parts := make([]string, 0, 4)
	if prefix != "" {
		parts = append(parts, prefix)
	}
	if deployment != "" {
		parts = append(parts, deployment)
	}
	if job != "" {
		parts = append(parts, job)
	}
	if index != "" {
		parts = append(parts, index)
	}
	if len(parts) == 0 {
		return ""
	}
	return sanitizeVMName(strings.Join(parts, "-"))
}
