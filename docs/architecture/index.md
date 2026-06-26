# The BOSH PVE CPI — An Architecture, From First Principles

This is the architecture of the BOSH PVE CPI told as a story. It starts from the problem the component exists to solve and derives every feature from a stated principle, so that by the end the whole design feels inevitable rather than assembled. We can read it front to back like a short book, or present it top to bottom in a meeting — the chapters are ordered so that each one earns the next.

## What this is, and how it differs from the other docs

The reference documentation in the [parent directory](../index.md) describes the finished implementation from the bottom up: what each method does, what each setting controls, how each subsystem behaves. It answers "how does it work?"

This document goes the other way. It treats the implementation as a working prototype and re-derives the architecture from fundamentals — problem first, principle next, feature last. It answers "why is it built this way, and why is that the right way?" Where the reference docs name functions and fields, here we name only capabilities and the reasoning behind them. The audience is anyone with a semi-technical background: an architect, an engineering manager, a new team member. No prior knowledge of BOSH, Proxmox VE, or Go is assumed.

Each chapter opens with the first principle it derives from and carries diagrams that advance the story. At the end, it links back to the reference docs for anyone who wants the mechanism.

## The arc

The thirteen chapters fall into four parts plus a closing synthesis.

**Part I — Foundations.** The contract, and the single constraint everything descends from.

- **[Chapter 1 — The Problem and the Seam](01-problem-and-seam.md)**
Why a general orchestrator that refuses to learn any specific cloud needs a translator, and why Proxmox VE is not a cloud but a hypervisor the CPI must turn into one.

- **[Chapter 2 — One Constraint, Many Consequences](02-stateless-contract.md)**
The Director runs this binary once per request and may retry it — and from that one fact the whole stateless, idempotent, honest contract unfolds.

**Part II — Building a Machine.** One VM's life, from golden image to a running instance that phones home.

- **[Chapter 3 — The Lifecycle of a Machine](03-lifecycle.md)**
A deploy is a sequence of small, retriable operations; the CPI is the vocabulary the Director sequences, with VM and data lifecycles deliberately kept apart.

- **[Chapter 4 — The Stemcell as a Mold](04-stemcell-mold.md)**
Pay the image cost once and clone in seconds instead of minutes, with an identity scheme that refuses to import the same image twice.

- **[Chapter 5 — Giving a Machine Its Identity](05-machine-identity.md)**
A fresh clone is anonymous; we hand it its identity out of band, on a sealed disc the guest already knows how to read, with no login and no registry.

**Part III — Cloud Primitives PVE Lacks.** The three big inventions, each manufacturing something the hypervisor does not provide.

- **[Chapter 6 — Manufacturing a Scheduler](06-scheduler.md)**
Building a placement engine out of a live cluster read, separating soft preference from hard fault-domain constraint, and knowing when to hand placement back.

- **[Chapter 7 — Portable Networks](07-portable-networks.md)**
Making a network identity as portable as the workload it serves, and the war story of why a network we do not fully own can quietly break everything.

- **[Chapter 8 — Inventing the Durable Volume](08-durable-volume.md)**
PVE has no durable volume, so we build one out of a string — and give it a visible, protected home when it has no machine to live on.

**Part IV — Living in Production.** What it takes to survive real load on an inelastic platform.

- **[Chapter 9 — Absorbing the Storm](09-absorbing-the-storm.md)**
The CPI as an impedance-matching transformer between an elastic orchestrator and a hypervisor built for a handful of calls, not a fan-out storm.

- **[Chapter 10 — Never Wedge, Never Leak, Never Lose](10-safety.md)**
The safety floor: compose all-or-nothing operations from non-atomic parts, serialize a cluster-wide change with no native lock, and refuse to destroy data we cannot prove is ours to destroy.

- **[Chapter 11 — Hostile by Default](11-hostile-by-default.md)**
Least privilege derived from the call graph, secrets that never reach a log, and extension points designed as if every input were an attacker.

- **[Chapter 12 — Operating the Thing](12-operating.md)**
Designing the component so the person debugging it at three in the morning can find the cause, and proving that an upgrade changes nothing unless asked.

**Closing.**

- **[Chapter 13 — The Whole Picture](13-whole-picture.md)**
The recurring principles seen together, the layered architecture that emerges from them rather than being imposed, and an honest map of where the walls are.

## Presenting this in a meeting

The chapters are sized and ordered for a top-to-bottom walkthrough. A few ways to use them:

- **Full review**
Walk all thirteen in order. Each chapter is roughly one to two slides of narrative plus one or two diagrams, and every chapter opens with the problem it solves, so the room always knows why we are here before we discuss how.

- **Executive pass**
Chapter 1, the first principle line at the top of Chapters 2 through 12, and Chapter 13. That gives the why, the spine, and the synthesis in a fraction of the time.

- **Topic deep-dive**
Jump to a single Part — placement, networking, storage, resilience, safety — using the part headings above. Each chapter is self-contained enough to stand alone, and links forward and back where context helps.

## Grounding in the implementation

For mechanism-level detail behind any chapter, the reference documentation lives one level up at [the documentation index](../index.md).
