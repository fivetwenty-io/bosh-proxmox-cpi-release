# Image Generation Prompts

This file records the image-generation prompts and prompt briefs used for the
architecture presentation visuals.

Source note:

- `Exact`: captured from the current session context.
- `Reconstructed`: the original prompt text was not available after context
  compaction, so this records the concept brief used for the asset and preserves
  the same visual language.
- `Extra downloaded asset`: present in the assets directory, but not currently
  referenced by a slide.

Shared style target used throughout:

```text
Polished 3D isometric technical illustration, clean white studio background,
glass and brushed metal surfaces, premium engineering diagram aesthetic.
Match the existing deck visuals: cool whites, pale blue light trails, brushed
graphite metal, and warm gold highlights. No text, no labels, no logos, no
watermark, no humans, no dark background, no purple gradient, no clutter.
```

## Slide Visuals

### `cover-seam.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation cover visual, 16:9 landscape.
Primary request: A clean abstract seam between a generic orchestrator cloud and
a Proxmox-style infrastructure cluster, connected through a precise adapter
device. Show generic intent on one side, platform-specific machinery on the
other, with the adapter as the central translation point.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, generous clean margins, adapter centered.
Lighting/mood: bright, calm, precise, trustworthy.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `seam-adapter.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A precise adapter connecting generic orchestration intent to
platform-specific infrastructure calls. Show a cloud/orchestrator source on one
side, a compact adapter in the middle, and a cluster of infrastructure blocks on
the other side.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, left-to-right flow, adapter clearly central.
Lighting/mood: bright, calm, precise, architectural.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `primitive-factory.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: An abstract factory that manufactures missing cloud primitives
from raw hypervisor parts. Show a compact technical workshop stamping out small
golden blocks representing scheduler, network, volume, and identity primitives.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, factory/workshop center, output primitives
flowing outward.
Lighting/mood: bright, calm, precise, capable.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `wire-shape.png`

Status: Reconstructed from current-session brief

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: Show one request packet entering a stateless adapter and one
response packet leaving. Represent JSON-RPC one-in, one-out semantics without
showing readable text.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, compact adapter centered, blue request beam in,
gold response beam out.
Lighting/mood: bright, calm, precise.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `stateless-live-state.png`

Status: Reconstructed from current-session brief

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A stateless adapter with an empty memory chamber reading live
cluster state and returning an identity token. The Director keeps the map; the
CPI reads the territory.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, live-state cluster on one side, empty adapter in
the middle, returned token on the other side.
Lighting/mood: bright, calm, precise.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `idempotent-retry.png`

Status: Reconstructed from current-session brief

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: Repeated retry arrows converge on one existing VM/resource
block. Ghost duplicate attempts fade away, showing reuse and convergence rather
than duplication.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, multiple blue retry paths converging on a single
stable golden resource.
Lighting/mood: bright, calm, precise.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `read-back-reconcile.png`

Status: Reconstructed from current-session brief

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: An orchestrator ledger comparing records against live
infrastructure via a scan beam, matching existing items and revealing missing or
orphaned resources.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, ledger/console left, live infrastructure blocks
right, scan beam between them.
Lighting/mood: bright, calm, precise, diagnostic.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `durable-volume.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: Show persistent durable data surviving beside replaceable
compute. Use a protected golden storage capsule as the stable object and smaller
disposable compute blocks nearby.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, durable volume prominent, compute secondary.
Lighting/mood: bright, calm, protective.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `disk-metadata-carriers.png`

Status: Reconstructed from current-session brief

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A CPI workshop translating missing per-disk primitives into
VM-level carriers. Show a disk capsule, a VM snapshot plate, and tag/description
tokens being assembled by technical robotic arms.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, disk capsule and VM carrier objects connected
through a compact workshop.
Lighting/mood: bright, calm, precise, inventive.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `stemcell-mold.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A golden stemcell template used as a mold or press to stamp
many cloned VM blocks. Communicate "import once, stamp many" and a move from
minutes to seconds without readable text.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, mold/press central, cloned blocks flowing out.
Lighting/mood: bright, calm, efficient.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `content-fingerprint.png`

Status: Reconstructed from current-session brief

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: Identical golden image blocks matched by a central
fingerprint/hash token. Duplicates converge into one canonical template.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, several candidate blocks feeding into a central
fingerprint token, one canonical block on the far side.
Lighting/mood: bright, calm, precise.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `template-scaffolding.png`

Status: Exact

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: An abstract technical illustration showing a golden template
block as temporary build-time scaffolding, with lightweight construction rails
and supports being removed, while several cloned VM cubes remain stable and
independent after creation.
Style/medium: polished 3D isometric technical illustration, clean white studio
background, glass and brushed metal surfaces, subtle depth, premium engineering
diagram aesthetic.
Composition/framing: wide 16:9, central template/scaffold on the left-middle,
stable cloned VM cubes extending to the right, generous clean margins for slide
layout.
Lighting/mood: bright, calm, precise, trustworthy.
Color palette: match existing deck visuals: cool whites, pale blue light
trails, brushed graphite metal, small warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans, no dark
background, no purple gradient, no noisy detail.
```

### `template-replication.png`

Status: Exact

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: An abstract technical illustration showing a single golden
stemcell/template artifact being replicated across multiple Proxmox cluster
nodes through a shared storage fabric, with synchronized copies lighting up on
each node.
Style/medium: polished 3D isometric technical illustration, clean white studio
background, glass and brushed metal surfaces, premium engineering diagram
aesthetic.
Composition/framing: wide 16:9, shared storage/fabric line across the lower
center, three or four small server nodes across the scene, golden template
copies glowing consistently on each node, generous margins.
Lighting/mood: bright, calm, precise, resilient.
Color palette: match existing deck visuals: cool whites, pale blue network
lines, brushed graphite metal, warm gold highlights for template artifacts.
Constraints: no text, no labels, no logos, no watermark, no humans, no dark
background, no purple gradient, no clutter.
```

### `cluster-wide-replication.png`

Status: Extra downloaded asset

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A cluster-wide replication or carrier-workshop concept in the
same visual system. This downloaded asset is not currently referenced by a
slide; preserve it as an alternate for replication/storage-carrier slides.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9 with clean margins.
Lighting/mood: bright, calm, precise.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `configdrive-identity.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A sealed ConfigDrive identity envelope attached to a machine,
showing identity delivered out-of-band rather than through a conversation with
the guest.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, machine block with a sealed capsule/envelope
beside it, gentle blue/gold connection.
Lighting/mood: bright, calm, secure.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `scheduler-selection.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A synthesized scheduler selecting among several nodes, with
placement signals, soft preferences, and durable selection visible as an
architectural control tower or decision surface.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, scheduler/decision tower foreground, several
node blocks around it.
Lighting/mood: bright, calm, deliberate.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `portable-network.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A portable network identity carried across multiple nodes,
with several server blocks connected by a shared blue network fabric and a
golden workload/address token that can move.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, nodes across the scene, shared network fabric
linking them.
Lighting/mood: bright, calm, mobile.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `storm-buffer.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A request storm transformed into controlled work output. Show
many blue incoming lines entering a buffering transformer and fewer ordered
golden lines leaving toward infrastructure.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, dense input on left, transformer/buffer center,
controlled output right.
Lighting/mood: bright, calm, controlled.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `rollback-safety.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: Ordered undo tokens beside a partially built machine, showing
last-acquired-first-released rollback and safe cleanup after failure.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, partially built machine/workshop with a clear
reverse chain of golden undo tokens.
Lighting/mood: bright, calm, safe.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `least-privilege.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: Least privilege and explicit extension points shown as a
locked core with only specific allowed connectors reaching outward. Forbidden
paths are absent or visibly blocked.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, protected core center, permitted connectors as
neat blue/gold paths.
Lighting/mood: bright, calm, security-focused.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `operations-diagnosis.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: Evidence streams converging into a diagnostic surface. Show
configuration, RPC, and log-like artifacts feeding a central diagnosis console
that highlights one actionable cause.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, evidence sources around a central console.
Lighting/mood: bright, calm, investigative.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `whole-picture-factory.png`

Status: Reconstructed

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid, 16:9 landscape.
Primary request: A small factory standing between an orchestrator cloud and a
hypervisor cluster, manufacturing several missing cloud primitives on demand.
Style/medium: polished 3D isometric technical illustration.
Composition/framing: wide 16:9, factory/control module center, cloud input on
one side and infrastructure output on the other.
Lighting/mood: bright, calm, complete.
Color palette: cool whites, pale blue light trails, brushed graphite metal,
warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans.
```

### `paperwork-race.png`

Status: Exact

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid for slide 49, 16:9
landscape.
Primary request: An abstract technical illustration showing the physical disk
truth staying stable while advisory paperwork/metadata cards race and blur
beside it. The disk attachment is locked and durable; a small note/record trail
is visibly secondary and best-effort.
Style/medium: polished 3D isometric technical illustration, clean white studio
background, glass and brushed metal surfaces, premium engineering diagram
aesthetic.
Composition/framing: wide 16:9. Place a protected golden disk capsule or
storage block as the solid anchor on the right-center, with lighter translucent
metadata cards or paper-like panels sweeping/racing on the left. Use clear
visual hierarchy: physical fact is solid; paperwork is softer and moving.
Lighting/mood: bright, calm, precise, trustworthy.
Color palette: match existing deck visuals: cool whites, pale blue light
trails, brushed graphite metal, warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans, no dark
background, no purple gradient, no clutter.
```

### `intent-altitude.png`

Status: Exact

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid for slide 50, 16:9
landscape.
Primary request: An abstract technical illustration showing intent expressed at
layered altitude: global defaults, profile settings, and per-call/per-disk
options stacking into one final storage decision. The most specific layer wins
and resolves into a selected storage tier/capability.
Style/medium: polished 3D isometric technical illustration, clean white studio
background, glass and brushed metal surfaces, premium engineering diagram
aesthetic.
Composition/framing: wide 16:9. Show three translucent stacked glass plates or
control layers descending from left to right into a central resolver prism, then
a single golden selected storage block on the right. Use blue lines for
broad/global inputs and gold for the final winning specific intent.
Lighting/mood: bright, calm, precise, architectural.
Color palette: match existing deck visuals: cool whites, pale blue light
trails, brushed graphite metal, warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans, no dark
background, no purple gradient, no clutter.
```

### `host-side-limits.png`

Status: Exact

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid for slide 55, 16:9
landscape.
Primary request: An abstract technical illustration showing that the fix is
host-side: several CPI/API request streams meet hard host-side infrastructure
limits, with separate storage lockfiles and worker lanes. One split storage lane
relieves pressure while a hard 30-second wall remains visible as an immovable
boundary.
Style/medium: polished 3D isometric technical illustration, clean white studio
background, glass and brushed metal surfaces, premium engineering diagram
aesthetic.
Composition/framing: wide 16:9. On the left, many pale blue request beams; in
the middle, a small host/control chassis with worker slots and two separate
lockfile/rail lanes; on the right, a firm transparent wall or gate representing
a platform limit. Show relief through separated lanes, not chaos.
Lighting/mood: bright, calm, precise, operational.
Color palette: match existing deck visuals: cool whites, pale blue light
trails, brushed graphite metal, warm gold highlights.
Constraints: no text, no numbers, no labels, no logos, no watermark, no humans,
no dark background, no purple gradient, no clutter.
```

### `lifecycle-boundary.png`

Status: Exact

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid for slide 60, 16:9
landscape.
Primary request: An abstract technical illustration showing a lifecycle boundary
that delete_vm cannot cross: a disposable VM block is removed on one side, while
protected persistent disk capsules remain behind a separate namespace boundary
and locked guard rail. The system fails closed rather than crossing into data
destruction.
Style/medium: polished 3D isometric technical illustration, clean white studio
background, glass and brushed metal surfaces, premium engineering diagram
aesthetic.
Composition/framing: wide 16:9. Place a fading/removed compute cube on the left,
a clear vertical glass boundary in the center, and protected golden disk
capsules on the right with a lock/guard motif. Use a subtle stop/blocked path
without text.
Lighting/mood: bright, calm, precise, safety-focused.
Color palette: match existing deck visuals: cool whites, pale blue light
trails, brushed graphite metal, warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans, no dark
background, no purple gradient, no clutter.
```

### `symptom-index.png`

Status: Exact

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid for slide 70, 16:9
landscape.
Primary request: An abstract technical illustration showing operations diagnosis
indexed by observable symptom: a visible repeating pulse/flap signal enters a
diagnostic index or runbook console, evidence packets and packet-capture beams
converge, and one root-cause class is isolated cleanly.
Style/medium: polished 3D isometric technical illustration, clean white studio
background, glass and brushed metal surfaces, premium engineering diagram
aesthetic.
Composition/framing: wide 16:9. On the left, rhythmic blue pulse lines from
several small instance cubes; in the center, an open diagnostic index/runbook
console or tablet with organized cards; on the right, a single golden
highlighted cause module separated from faded noise. No readable text.
Lighting/mood: bright, calm, precise, investigative.
Color palette: match existing deck visuals: cool whites, pale blue telemetry
lines, brushed graphite metal, warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans, no dark
background, no purple gradient, no clutter.
```

### `architecture-walls.png`

Status: Exact

```text
Use case: productivity-visual
Asset type: Slidev architecture presentation visual aid for slide 75, 16:9
landscape.
Primary request: An abstract technical illustration showing honest architecture
walls and constraints: several transparent boundary walls or gates mark real
platform limits around a small infrastructure factory, with paths stopping or
bending deliberately instead of pretending the walls are gone.
Style/medium: polished 3D isometric technical illustration, clean white studio
background, glass and brushed metal surfaces, premium engineering diagram
aesthetic.
Composition/framing: wide 16:9. Center a compact factory/control module. Around
it, five subtle transparent glass walls/gates representing hard constraints.
Blue/gold paths approach, some pass through approved openings, others stop
cleanly at closed boundaries. The mood should be honest and engineered, not
alarming.
Lighting/mood: bright, calm, precise, candid.
Color palette: match existing deck visuals: cool whites, pale blue light
trails, brushed graphite metal, warm gold highlights.
Constraints: no text, no labels, no logos, no watermark, no humans, no dark
background, no purple gradient, no clutter.
```
