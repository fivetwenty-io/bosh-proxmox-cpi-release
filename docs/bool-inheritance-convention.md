# Bool Inheritance Convention

Optional boolean knobs in this CPI follow a six-level inheritance chain. Every
level that is absent or `nil` falls through to the next; the chain ends at a
hard-coded default (almost always `false`). The result is **byte-identical
behavior** when no level is set — no migration required.

## The Six Levels

| Level | Source | Type |
|-------|--------|------|
| 1 | Per-call `cloud_properties` | `bool` / `"true"` / `1` (JSON-coerced) |
| 2 | Per-disk-type profile `cloud_properties` (`cloud_properties.disk_type`) | same |
| 3 | Per-VM-type profile `cloud_properties` (`cloud_properties.vm_type`) | same |
| 4 | Per-request `pve_*` context override (BOSH cpi-config `properties:` flattened into the request context; `config.ApplyContextOverrides`) | JSON-coerced onto the effective `CPIConfig` |
| 5 | Global `CPIConfig` field | `*bool` |
| 6 | Hardcoded default | `bool` constant (typically `false`) |

Level 1 is the per-call override. Levels 2 and 3 are the `vm_type`/`disk_type`
profile layers managed by `layeredResolver`. Level 4 rewrites the effective
`CPIConfig` for one dispatched request from the cpi-config entry's properties —
see [Design Decisions — D12](design-decisions.md#d12--per-request-cloud-property-overrides-from-cpi-config).
The override set is closed (`contextOverrideFieldOrder` in
`internal/config/context_overrides.go`); for booleans it covers
`pve_verify_ssl` and `pve_stemcell_replicate_local`. Level 5 is a `*bool` field on
`CPIConfig`; a `nil` pointer here distinguishes "not configured" from an explicit
`false`. Level 6 is the constant the CPI shipped with before the feature existed.

## Resolution Pattern

### Profile layers (Levels 1–3): `r.Bool(key)`

`layeredResolver.Bool()` in
`internal/cpi/handlers/cloudprops_resolver.go` walks the three
cloud-properties layers (call → disk_type profile → vm_type profile) in
precedence order, coercing each value to `bool` via `coerceBool` (accepts
`bool`, `float64` `1`/`0`, `int` `1`/`0`, and case-insensitive string
`"true"`/`"false"`/`"1"`/`"0"`). The first parseable value wins; explicit
`false` is distinguished from absent. If no layer yields a parseable value,
`Bool` returns `(false, false)`.

### Global config fallback (Level 5): `*bool` field

Global config fields that are optional booleans use `*bool` so `nil` (field
absent from JSON) is distinguishable from an explicit `false`. Accessor
methods on `*CPIConfig` dereference safely:

```go
func (c *CPIConfig) VMFirewallEnabled() bool {
    return c.VMFirewall != nil && *c.VMFirewall
}
```

Existing examples: `VMFirewall`, `VerifySSL`, `EnsureNoIPConflicts`,
`ResizeWaitForConvergence`, `FastPathDelete`, `RedactLogs`, `AntiAffinityVerify`,
`TaskPollAdaptive`.

### Combining levels

A handler that supports all six levels writes:

```go
// Resolve from call/disk_type/vm_type layers first.
if v, ok := r.Bool("iothread"); ok {
    // Level 1-3: explicit per-call or profile value.
    if v {
        opts["iothread"] = "1"
    }
} else if dp := deps.Config.DiskPerformance; dp != nil && dp.Iothread != nil {
    // Level 5: global config *bool (already carrying any Level-4 override).
    if *dp.Iothread {
        opts["iothread"] = "1"
    }
}
// Level 6 (implicit): absent → iothread's hardcoded default (true — see the
// exception note below); most other keys default false.
```

The disk-performance resolver in
`internal/cpi/handlers/disk_performance.go` (`resolveDiskPerfOptions`,
`resolveDiskPerfBool`) implements this pattern for `iothread`, `ssd`, and
`discard`. `resolveDiskPerfBool` takes an explicit `defaultVal` parameter so
Level 6 can differ per key rather than being a single package-wide constant.

**Exception: `iothread` and `virtio_scsi_single` default to `true`.** These
two knobs' Level 6 default is a static `true` rather than `false`. See
[Configuration — Disk Performance](configuration.md#disk-performance) for the
full rationale and the drift-governance behavior for disks created before
that default changed.

**Exception: `discard` and `ssd` have a *computed* Level 6, not a constant.**
Rather than a fixed `true` or `false`, their Level 6 default is
`pve.IsTrimCapable(storageType, format)` — the disk's actual resolved storage
pool's TRIM capability, evaluated once per disk at bake time. An explicit
value at any of Levels 1–5 still wins over this computed default exactly as
it would over a constant one; only the *fallback itself* differs from the
rest of this convention. See [Configuration — Discard/SSD
auto-resolution](configuration.md#discardssd-auto-resolution) for the full
TRIM-capability matrix.

## Byte-Identical Guarantee

When all six levels are `nil`, absent, or unset, the effective value equals
the Level 6 constant. No behavior change occurs relative to any prior release
that lacked the feature. This property holds because:

- `*bool` fields on `CPIConfig` have `omitempty` JSON tags, so they are never
  written to the ERB output unless explicitly set.
- `layeredResolver.Bool` returns `(false, false)` when no layer matches, not
  `(false, true)`.
- Accessor methods such as `VMFirewallEnabled()` return `false` for both a `nil`
  pointer and a `*false` pointer.

A deployment that adds no new spec properties gets exactly the behavior it had before the feature was merged.

## Adding a New Optional Bool

1. Add a `*bool` field to `CPIConfig` with `json:"...,omitempty"`.
2. Write an accessor `FooEnabled() bool` that returns `c.Foo != nil && *c.Foo`.
3. In the handler, call `r.Bool("foo")` first; fall back to
   `cfg.FooEnabled()` when `r.Bool` returns `ok=false`.
4. Add the spec key with `default: ~` (nil BOSH default) so the ERB emits the
   field only when explicitly set.
5. In the ERB, emit the key only when non-nil (mirrors `pve.verify_ssl`
   and `vm_firewall` patterns).
6. Write a test asserting that a `CPIConfig{}` with no fields set produces the
   same behavior as before the feature existed.

## Known Deviations

- `VerifySSL` defaults to `true` via `VerifySSLValue()` (the safer default for a
  TLS field), not `false`. The six-level chain still applies; Level 6 is `true`.
  `pve_verify_ssl` is also a Level-4 override key, so one cpi-config entry can
  differ from the job-level setting.
- `PlacementConfig.ExcludeMaintenanceNodes` defaults to `true` for the same
  reason (protective default).
- `PlacementConfig.PinAZStrict` defaults to `true` when `PinAZViaHARules` is enabled.

These fields use accessor methods that encode the Level 6 constant explicitly
rather than relying on the Go zero value.
