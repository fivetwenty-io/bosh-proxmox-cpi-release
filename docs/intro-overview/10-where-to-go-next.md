# Chapter 10 — Where to Go Next

*Minutes 52–55 of the hour, then questions.*

Everything this hour compressed into five-minute chapters exists somewhere in `docs/` at full depth, written for the moment a topic stops being an overview and becomes an afternoon's work. This closing map is organized the way the questions tend to arrive.

*The idea this chapter rests on: nobody should need to remember the details from today — only where they live.*

## The map

- **Setting up for the first time**
`docs/pve-settings.md` for the one-time cluster prerequisites, `docs/pve-api-permissions.md` for creating the scoped credential step by step, and the README's quickstart for standing up a Director. `docs/emptyvm.md` is the minimal smoke deployment — one stemcell VM, no jobs — that proves the whole path works before anything real rides on it.

- **Configuration, in full**
`docs/configuration.md` is the complete property reference, ending with the zero-config baseline table — the authoritative list of what is on by default and why. `docs/examples.md` holds worked manifests.

- **Networking**
`docs/networks.md` covers both the borrowed-bridge default and managed SDN in depth, including VLANs, MTU, and the isolated-network pattern from the war story.

- **Storage and disks**
`docs/persistent-disks.md` for how disks map onto storage backends, and `docs/persistent-disk-strategy.md` for the detached-disk story — free-floating versus parked — and the disk-audit tool.

- **Day two**
`docs/operations.md` is the runbook: log access, health checks, the pre-deploy checklist, orphan cleanup. `docs/troubleshooting.md` is the symptom-indexed triage guide. `docs/ha-and-resurrection.md` settles the one-healer question with an ownership matrix and measured timings.

- **The deep why**
`docs/architecture/` — thirteen chapters deriving this whole design from first principles, for anyone who enjoyed today's story and wants the unabridged edition. `docs/design-decisions.md` records the judgment calls and the alternatives they beat.

## The habits worth forming

If only three things survive contact with next week, let them be these. The VMID bands make the cluster legible — 100s are machines, 9000s are disks, 30000s are templates — so read IDs before touching anything. A failed deploy's first response is `bosh cloud-check`, then re-deploy; retries are the design, not a workaround. And exactly one system heals a deployment — decide which, deliberately.

The floor is open.
