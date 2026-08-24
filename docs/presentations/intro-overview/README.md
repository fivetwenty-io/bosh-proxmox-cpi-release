# Intro-Overview Presentation

A [Slidev](https://sli.dev) deck introducing the BOSH Proxmox CPI to operators, built
entirely from the prose in [`../../intro-overview/`](../../intro-overview/). Ten
chapters sized for a one-hour meeting: roughly fifty-five minutes of talk plus
question time. Presented by Wayne E. Seguin, FiveTwenty Inc.

The slides are a visual aid only — minimal cues per slide, with the narrative read
aloud from the corresponding `docs/intro-overview/` chapter. The audience is
operators who are not quite developers; nothing on a slide assumes the ability to
read code.

## Layout

- `slides.md` — the deck entry point: headmatter, title, agenda, and `src:` imports of every chapter.
- `01-…` through `10-…` — one file per chapter (transition slide + section slides). Each mirrors a source chapter.
- `style.css` — global styling shared with the architecture deck: vertically balanced bullet slides and centered diagrams. Diagram *sizing* is per-slide: Slidev 52+ renders Mermaid inside a shadow root that page CSS cannot reach, so each fence carries a ` ```mermaid {scale: N}` option tuned to fit its slide.

The repository-root `package.json` pins `@slidev/cli` and `@slidev/theme-default`
(alongside the `playwright-chromium` the PDF export uses), so the deck builds on a
fresh clone; the make targets run `bun install` before Slidev.

The deck font is **Nunito** (FiveTwenty's main font), set via the `fonts:` headmatter
in `slides.md` and pulled from Google Fonts at build time.

## Run it

We use bun. From the repository root, via the Makefile:

```bash
make slides-intro-overview          # live presenter dev server (opens the browser, hot-reloads)
make slides-intro-overview-export   # export to docs/presentations/intro-overview/intro-overview.pdf
make slides-intro-overview-build    # build a static single-page app to ./dist
```

Or directly with bunx from this directory:

```bash
bunx @slidev/cli slides.md
bunx @slidev/cli export slides.md --output intro-overview.pdf
bunx @slidev/cli build slides.md --out dist
```

Mermaid diagrams and the per-chapter files are pulled in automatically by `slides.md`;
no extra wiring is needed. Build artifacts (`dist/`, `intro-overview.pdf`, `node_modules/`)
are gitignored.

## Editing

Edit a chapter's slides in its own `NN-*.md` file. Keep slides terse — they cue the
talk, they do not contain it. The full narrative lives in `docs/intro-overview/`; when
that prose changes, update the matching slide cue here.

Each slide also carries **presenter notes** — the trailing `<!-- … -->` HTML comment
on the slide, shown only in Slidev's presenter view. These hold the talking points,
the per-chapter timing (also printed at the top of each script chapter), and answers
to the questions each slide tends to raise. They never render on the slide itself.
