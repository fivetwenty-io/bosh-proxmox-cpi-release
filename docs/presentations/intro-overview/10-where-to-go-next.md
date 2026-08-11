---
layout: section
---

# Chapter 10
## Where to Go Next

*Nobody needs to remember the details — only where they live.*

<!--
- Minutes 52–55, then questions. Everything compressed today exists at full depth in docs/.
-->

---

## The map

- **First-time setup** — `pve-settings`, `pve-api-permissions`, README quickstart, `emptyvm`
- **Configuration** — `configuration.md` (the full reference), `examples.md`
- **Networking** — `networks.md` · **Storage** — `persistent-disks.md`, `persistent-disk-strategy.md`
- **Day two** — `operations.md`, `troubleshooting.md`, `ha-and-resurrection.md`
- **The deep why** — `docs/architecture/` (thirteen chapters), `design-decisions.md`

<!--
- All paths are under docs/ in the release repo; docs/index.md is the annotated table of contents.
- emptyvm: one stemcell VM, no jobs — proves the whole path before anything real rides on it.
- configuration.md ends with the zero-config baseline table: the authoritative list of what is on by default and why.
- The architecture narrative is today's story unabridged, derived from first principles — for whoever wants the long version.
-->

---

## Three habits worth keeping

- **Read the bands** — 100s are machines, 9000s are disks, 30000s are templates
- **`bosh cloud-check`, then re-deploy** — retries are the design
- **One healer per deployment** — decide which, deliberately

# Questions

<!--
- Close on the habits, then open the floor. Reserve ~5 minutes minimum.
- Likely questions to be ready for: multi-cluster (one Director, several cpi-config entries, disjoint VMID bands), light stemcells, what PVE versions are supported (PVE 9.x; 9.2 only for the opt-in DLB), performance of clones on their storage backend.
-->
