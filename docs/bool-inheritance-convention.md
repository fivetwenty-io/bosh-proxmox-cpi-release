# Bool Inheritance Convention

Optional boolean knobs in this CPI follow a five-level inheritance chain. Every
level that is absent or `nil` falls through to the next; the chain ends at a
hard-coded default (almost always `false`). The result is **byte-identical
behavior** when no level is set — no migration required.

## The Five Levels

| Level | Source | Type |
|-------|--------|------|
| 1 | Per-call `cloud_properties` | `bool` / `"true"` / `1` (JSON-coerced) |
| 2 | Per-disk-type profile `cloud_properties` (`cloud_properties.disk_type`) | same |
| 3 | Per-VM-type profile `cloud_properties` (`cloud_properties.vm_type`) | same |
| 4 | Global `CPIConfig` field | `*bool` |
| 5 | Hardcoded default | `bool` constant (typically `false`) |

Level 1 is the per-call override. Levels 2 and 3 are the `vm_type`/`disk_type`
profile layers managed by `layeredResolver`. Level 4 is a `*bool` field on
`CPIConfig`; a `nil` pointer here distinguishes "not configured" from an explicit
`false`. Level 5 is the constant the CPI shipped with before the feature existed.

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

### Global config fallback (Level 4): `*bool` field

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

A handler that supports all five levels writes:

```go
// Resolve from call/disk_type/vm_type layers first.
if v, ok := r.Bool("iothread"); ok {
    // Level 1-3: explicit per-call or profile value.
    if v {
        opts["iothread"] = "1"
    }
} else if dp := deps.Config.DiskPerformance; dp != nil && dp.Iothread != nil {
    // Level 4: global config *bool.
    if *dp.Iothread {
        opts["iothread"] = "1"
    }
}
// Level 5 (implicit): absent → omit → PVE default (disabled).
```

The disk-performance resolver in
`internal/cpi/handlers/disk_performance.go` (`resolveDiskPerfOptions`,
`resolveDiskPerfBool`) implements this pattern for `iothread`, `ssd`, and
`discard`.

## Byte-Identical Guarantee

When all five levels are `nil`, absent, or unset, the effective value equals
the Level 5 constant. No behavior change occurs relative to any prior release
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
  TLS field), not `false`. The five-level chain still applies; Level 5 is `true`.
- `PlacementConfig.ExcludeMaintenanceNodes` defaults to `true` for the same
  reason (protective default).
- `PlacementConfig.PinAZStrict` defaults to `true` when `PinAZViaHARules` is enabled.

These fields use accessor methods that encode the Level 5 constant explicitly
rather than relying on the Go zero value.
