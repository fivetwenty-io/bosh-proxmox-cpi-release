# The BOSH PVE CPI — An Operator's Introduction

# What this is

This is the talking script for a one-hour introduction to the BOSH PVE CPI, written for operators. It explains how the system is put together, how it works, and how it is configured — in prose, without requiring anyone to read the implementation. Someone who follows the hour should be able to reason about a deployment, read the cluster, configure the CPI, and know exactly where to dig when a topic needs more depth. The code stays available for those who want it afterward; it is never required.

The script is the companion to the Slidev deck in [`docs/presentations/intro-overview/`](../presentations/intro-overview/). The slides carry minimal cues and diagrams; the narrative below is what gets said. Each chapter opens with the idea it rests on, in italics, and closes with a hand-off to the next — the same shape as the [architecture narrative](../architecture/index.md), at introductory altitude.

## The hour

Ten chapters, fifty-five minutes of talk, and the rest for questions. Timings are printed at the top of each chapter; they assume a conversational pace and survive a few minutes of drift.

| Clock | Chapter | What it covers |
|---|---|---|
| 0:00–0:05 | [1 — Why We Are Here](01-why-we-are-here.md) | BOSH meets Proxmox, the driver metaphor, and why this CPI manufactures rather than translates |
| 0:05–0:11 | [2 — The Cast of Characters](02-the-cast.md) | Director, stemcell, agent, cluster, and the CPI — a program, not a service |
| 0:11–0:17 | [3 — What Happens When We Deploy](03-what-a-deploy-does.md) | The chain of small retriable steps, data versus compute, and the VMID bands that make the cluster legible |
| 0:17–0:22 | [4 — Machines from a Mold](04-machines-from-a-mold.md) | Template cloning in seconds, and identity delivered on a sealed disc |
| 0:22–0:27 | [5 — Where Machines Land](05-where-machines-land.md) | The manufactured scheduler, availability zones as a written map, and one healer at a time |
| 0:27–0:33 | [6 — The Network Story](06-networks.md) | Borrowed bridges, managed cluster-wide networks, and owning the address space on a provided network |
| 0:33–0:39 | [7 — Disks That Outlive Their Machines](07-disks-that-outlive.md) | The invented durable volume, ID envelopes, the parked-disk coat-check, and the audit habit |
| 0:39–0:46 | [8 — How We Configure It](08-how-we-configure-it.md) | Five required properties, the scoped credential, layered settings, and the additive-upgrade promise |
| 0:46–0:52 | [9 — When Things Go Wrong](09-when-things-go-wrong.md) | Retriable versus terminal, where truth lives, cloud-check, and who owns recovery |
| 0:52–0:55 | [10 — Where to Go Next](10-where-to-go-next.md) | The documentation map and the three habits worth keeping |

## Audience and use

The audience is operators who are not quite developers: comfortable with BOSH manifests, cloud configs, and a terminal, but not expected to read Go. The script deliberately trades implementation detail for operational understanding — property names and commands appear where an operator would actually type them, and nowhere else.

A few ways to use it:

- **The full hour**
Read the chapters aloud in order against the deck. Every chapter opens with why it matters before how it works, so the room is never asked to hold an unmotivated detail.

- **A shorter briefing**
Chapters 1, 3, 8, and 9 form a self-contained thirty-minute arc: the idea, the deploy, the configuration, and day two.

- **Self-study**
The chapters stand alone as prose and link into the deeper references at every point where the hour had to stop early. The closing chapter is the map.
