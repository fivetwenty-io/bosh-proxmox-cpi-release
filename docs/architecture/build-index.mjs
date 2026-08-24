#!/usr/bin/env bun
// Compile docs/architecture/*.md into a single clean, readable index.html.
//
// - index.md leads, then chapters 01..13 in order.
// - ```mermaid``` fences render client-side via the mermaid ESM build.
// - Intra-doc `NN-*.md` links are rewritten to in-page anchors.
// - A sidebar table of contents is generated from each file's H1.
//
// markdown-it is resolved by Bun's auto-install — no committed node_modules.
// Run via `make docs-architecture-html` (bun only, never npm/npx).

import { readFileSync, writeFileSync, readdirSync } from "node:fs";
import { dirname, join, basename } from "node:path";
import { fileURLToPath } from "node:url";
import MarkdownIt from "markdown-it";

const here = dirname(fileURLToPath(import.meta.url));

// Ordered source set: index first, then NN-*.md ascending.
const chapters = readdirSync(here)
  .filter((f) => /^\d\d-.*\.md$/.test(f))
  .sort();
const files = ["index.md", ...chapters];
const localSet = new Set(files);
const slug = (file) => basename(file, ".md");

const md = new MarkdownIt({
  html: true,
  linkify: true,
  typographer: true,
});

// Render ```mermaid``` blocks as <pre class="mermaid"> so mermaid.js picks them up.
const esc = (s) =>
  s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
const defaultFence =
  md.renderer.rules.fence ||
  ((tokens, idx, options, env, self) => self.renderToken(tokens, idx, options));
md.renderer.rules.fence = (tokens, idx, options, env, self) => {
  const token = tokens[idx];
  const info = (token.info || "").trim().split(/\s+/)[0];
  if (info === "mermaid") {
    return `<pre class="mermaid">${esc(token.content)}</pre>\n`;
  }
  return defaultFence(tokens, idx, options, env, self);
};

// Rewrite links to sibling chapter files into same-page anchors.
const rewriteLinks = (html) =>
  html.replace(
    /href="(?:\.\/)?([0-9A-Za-z._-]+\.md)(#[^"]*)?"/g,
    (whole, file) => (localSet.has(file) ? `href="#${slug(file)}"` : whole),
  );

const firstH1 = (text) => {
  const m = text.match(/^#\s+(.+?)\s*$/m);
  return m ? m[1] : null;
};

const sections = [];
const toc = [];
for (const file of files) {
  const raw = readFileSync(join(here, file), "utf8");
  const id = slug(file);
  const title = firstH1(raw) || id;
  toc.push({ id, title, isIndex: file === "index.md" });
  const body = rewriteLinks(md.render(raw));
  sections.push(`<section id="${id}" class="doc">\n${body}\n</section>`);
}

const tocHtml = toc
  .map(
    (t) =>
      `<li class="${t.isIndex ? "toc-index" : "toc-chapter"}"><a href="#${t.id}">${md.utils.escapeHtml(
        t.title,
      )}</a></li>`,
  )
  .join("\n");

const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>The BOSH Proxmox CPI — An Architecture, From First Principles</title>
<style>
  :root {
    --ink: #1c2330;
    --muted: #5a6678;
    --rule: #e4e8ef;
    --link: #2563a8;
    --bg: #ffffff;
    --sidebar: #f7f9fc;
    --code-bg: #f4f6fa;
    --max: 820px;
    --sidebar-w: 300px;
  }
  * { box-sizing: border-box; }
  html { scroll-behavior: smooth; }
  body {
    margin: 0;
    color: var(--ink);
    background: var(--bg);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    font-size: 17px;
    line-height: 1.65;
  }
  a { color: var(--link); text-decoration: none; }
  a:hover { text-decoration: underline; }
  .layout { display: flex; align-items: flex-start; }
  nav.toc {
    position: sticky;
    top: 0;
    width: var(--sidebar-w);
    flex: 0 0 var(--sidebar-w);
    height: 100vh;
    overflow-y: auto;
    padding: 2rem 1.25rem;
    background: var(--sidebar);
    border-right: 1px solid var(--rule);
    font-size: 0.92rem;
  }
  nav.toc h2 {
    font-size: 0.78rem;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: var(--muted);
    margin: 0 0 0.75rem;
  }
  nav.toc ol { list-style: none; margin: 0; padding: 0; counter-reset: ch; }
  nav.toc li { margin: 0.15rem 0; }
  nav.toc li.toc-index a { font-weight: 700; }
  nav.toc li.toc-chapter { counter-increment: ch; }
  nav.toc a { color: var(--ink); display: block; padding: 0.2rem 0; line-height: 1.35; }
  nav.toc a:hover { color: var(--link); text-decoration: none; }
  main {
    flex: 1 1 auto;
    min-width: 0;
    padding: 2.5rem 3rem 6rem;
    display: flex;
    justify-content: center;
  }
  .content { width: 100%; max-width: var(--max); }
  section.doc { padding-top: 1.5rem; }
  section.doc + section.doc { border-top: 1px solid var(--rule); margin-top: 2.5rem; }
  h1 { font-size: 2rem; line-height: 1.2; margin: 1.4rem 0 1rem; font-weight: 800; }
  h2 { font-size: 1.45rem; margin: 2.2rem 0 0.8rem; font-weight: 700; }
  h3 { font-size: 1.15rem; margin: 1.8rem 0 0.6rem; font-weight: 700; }
  p { margin: 0.9rem 0; }
  em { color: var(--muted); }
  ul, ol { padding-left: 1.4rem; }
  li { margin: 0.3rem 0; }
  hr { border: 0; border-top: 1px solid var(--rule); margin: 2rem 0; }
  blockquote {
    margin: 1rem 0;
    padding: 0.4rem 1.1rem;
    border-left: 3px solid var(--rule);
    color: var(--muted);
  }
  code {
    font-family: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
    font-size: 0.88em;
    background: var(--code-bg);
    padding: 0.12em 0.38em;
    border-radius: 4px;
  }
  pre {
    background: var(--code-bg);
    padding: 1rem 1.1rem;
    border-radius: 8px;
    overflow-x: auto;
  }
  pre code { background: none; padding: 0; font-size: 0.85em; }
  table { border-collapse: collapse; width: 100%; margin: 1.2rem 0; font-size: 0.95em; }
  th, td { border: 1px solid var(--rule); padding: 0.5rem 0.7rem; text-align: left; }
  th { background: var(--sidebar); }
  pre.mermaid {
    background: transparent;
    padding: 1rem 0;
    text-align: center;
    line-height: normal;
  }
  pre.mermaid svg { max-width: 100%; height: auto; }
  @media (max-width: 900px) {
    .layout { flex-direction: column; }
    nav.toc {
      position: static;
      width: 100%;
      flex-basis: auto;
      height: auto;
      border-right: 0;
      border-bottom: 1px solid var(--rule);
    }
    main { padding: 1.5rem 1.25rem 4rem; }
  }
</style>
</head>
<body>
<div class="layout">
<nav class="toc">
<h2>Contents</h2>
<ol>
${tocHtml}
</ol>
</nav>
<main>
<div class="content">
${sections.join("\n\n")}
</div>
</main>
</div>
<script type="module">
  import mermaid from "https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.esm.min.mjs";
  mermaid.initialize({ startOnLoad: true, theme: "neutral", securityLevel: "loose" });
</script>
</body>
</html>
`;

const out = join(here, "index.html");
writeFileSync(out, html);
console.log(`✓ ${out} (${files.length} documents, ${html.length} bytes)`);
