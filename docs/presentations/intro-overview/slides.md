---
theme: default
title: The BOSH Proxmox CPI — An Operator's Introduction
info: |
  A one-hour operator introduction to the BOSH Proxmox CPI.
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

## An Operator's Introduction

<div class="pt-8 opacity-80">
Presented by Wayne E. Seguin, FiveTwenty Inc.
</div>

<!--
- One hour: how it is put together, how it works, how it is configured. No code required — the goal is the mental model.
- The full narrative lives in docs/intro-overview/; these slides are cues only.
-->

---
layout: default
class: agenda
---

# The Hour

<div class="agenda-grid">

<div>

**The Idea**

1 · Why We Are Here

2 · The Cast of Characters

3 · What Happens When We Deploy

**The Machinery**

4 · Machines from a Mold

5 · Where Machines Land

</div>

<div>

6 · The Network Story

7 · Disks That Outlive Their Machines

**Running It**

8 · How We Configure It

9 · When Things Go Wrong

10 · Where to Go Next

</div>

</div>

<!--
- Three movements: the idea (ch 1–3), the machinery (ch 4–7), running it (ch 8–10).
- ~55 minutes of talk; questions inline are welcome, and time is reserved at the end.
-->

---
src: ./01-why-we-are-here.md
---

---
src: ./02-the-cast.md
---

---
src: ./03-what-a-deploy-does.md
---

---
src: ./04-machines-from-a-mold.md
---

---
src: ./05-where-machines-land.md
---

---
src: ./06-networks.md
---

---
src: ./07-disks-that-outlive.md
---

---
src: ./08-how-we-configure-it.md
---

---
src: ./09-when-things-go-wrong.md
---

---
src: ./10-where-to-go-next.md
---
