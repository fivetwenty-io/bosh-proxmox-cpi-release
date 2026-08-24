# Architecture Presentation

A [Slidev](https://sli.dev) deck of the BOSH Proxmox CPI architecture, built entirely
from the prose in [`../../architecture/`](../../architecture/). One slide per section,
a transition slide per chapter, an agenda up front. Presented by Wayne E. Seguin,
FiveTwenty Inc.

The slides are a visual aid only — minimal cues per slide, with the narrative read
aloud from the corresponding `docs/architecture/` chapter.

## Layout

- `slides.md` — the deck entry point: headmatter, title, agenda, and `src:` imports of every chapter.
- `01-…` through `13-…` — one file per chapter (transition slide + section slides). Each mirrors a source chapter.
- `style.css` — global styling, auto-loaded by Slidev: vertically balanced bullet slides and centered diagrams. Diagram *sizing* is not set here: Slidev 52+ renders Mermaid inside a shadow root that page CSS cannot reach, and Mermaid's own inline `max-width` fits each diagram to the slide column. Every diagram in this deck is laid out `LR` and fits on that basis; the per-fence ` ```mermaid {scale: N}` option is the only lever if one ever needs explicit sizing.

The deck font is **Nunito** (FiveTwenty's main font), set via the `fonts:` headmatter
in `slides.md` and pulled from Google Fonts at build time.

## Run it

We use bun. From the repository root, via the Makefile:

```bash
make slides-architecture          # live presenter dev server (opens the browser, hot-reloads)
make slides-architecture-export   # export to docs/presentations/architecture/architecture.pdf
make slides-architecture-build    # build a static single-page app to ./dist
```

Or directly with bunx from this directory:

```bash
bunx @slidev/cli slides.md
bunx @slidev/cli export slides.md --output architecture.pdf
bunx @slidev/cli build slides.md --out dist
```

Mermaid diagrams and the per-chapter files are pulled in automatically by `slides.md`;
no extra wiring is needed. Build artifacts (`dist/`, `architecture.pdf`, `node_modules/`)
are gitignored.

## Editing

Edit a chapter's slides in its own `NN-*.md` file. Keep slides terse — they cue the
talk, they do not contain it. The full narrative lives in `docs/architecture/`; when
that prose changes, update the matching slide cue here.

Each slide also carries **presenter notes** — the trailing `<!-- … -->` HTML comment
on the slide, shown only in Slidev's presenter view. These hold the technical talking
points and the decisions (and rejected alternatives) behind each section, sourced from
the reference docs in `docs/`. They never render on the slide itself.
