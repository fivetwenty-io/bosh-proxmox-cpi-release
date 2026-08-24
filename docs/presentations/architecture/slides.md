---
theme: default
title: The BOSH Proxmox CPI — An Architecture, From First Principles
info: |
  An architecture of the BOSH Proxmox CPI, told from first principles.
  Presented by Wayne E. Seguin, FiveTwenty Inc.
author: Wayne E. Seguin
class: text-center
highlighter: shiki
fonts:
  sans: Nunito
  serif: Nunito
  weights: '400,600,700,800'
  italic: true
  provider: google
mermaid:
  theme: neutral
transition: slide-left
layout: cover
---

# The BOSH Proxmox CPI

## An Architecture, From First Principles

<div class="pt-8 opacity-80">
Presented by Wayne E. Seguin, FiveTwenty Inc.
</div>

---
layout: default
class: agenda
---

# Agenda

<div class="agenda-grid">

<div>

**I — Foundations**

1 · The Problem and the Seam
2 · One Constraint, Many Consequences

**II — Building a Machine**

3 · The Lifecycle of a Machine
4 · The Stemcell as a Mold
5 · Giving a Machine Its Identity

</div>

<div>

**III — Cloud Primitives PVE Lacks**

6 · Manufacturing a Scheduler
7 · Portable Networks
8 · Inventing the Durable Volume

**IV — Living in Production**

9 · Absorbing the Storm
10 · Never Wedge, Never Leak, Never Lose
11 · Hostile by Default
12 · Operating the Thing

**Closing**

13 · The Whole Picture

</div>

</div>

---
src: ./01-problem-and-seam.md
---

---
src: ./02-stateless-contract.md
---

---
src: ./03-lifecycle.md
---

---
src: ./04-stemcell-mold.md
---

---
src: ./05-machine-identity.md
---

---
src: ./06-scheduler.md
---

---
src: ./07-portable-networks.md
---

---
src: ./08-durable-volume.md
---

---
src: ./09-absorbing-the-storm.md
---

---
src: ./10-safety.md
---

---
src: ./11-hostile-by-default.md
---

---
src: ./12-operating.md
---

---
src: ./13-whole-picture.md
---
