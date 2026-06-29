#!/usr/bin/env node
/**
 * Generate llms.txt + llms-full.txt from the docs corpus.
 *
 *  - static/llms.txt       : the llms.txt index (title, blurb, link list with
 *                            one-line descriptions) per https://llmstxt.org
 *  - static/llms-full.txt  : every doc concatenated as plain markdown, for
 *                            full-context ingestion (and to feed Digitorn's
 *                            own RAG docs assistant later).
 *
 * Pure Node, no deps. URL derivation mirrors Docusaurus defaults: numeric
 * `NN-` segment prefixes are stripped, `index` maps to its folder, and a
 * frontmatter `slug:` (absolute, starting with `/`) overrides the path.
 */
import {
  readdirSync,
  readFileSync,
  writeFileSync,
  mkdirSync,
} from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");
const DOCS = join(ROOT, "docs");
const OUT = join(ROOT, "static");
const SITE = "https://docs.digitorn.ai";
const BASE = "/docs";

const SITE_TITLE = "Digitorn";
const SITE_BLURB =
  "Declarative AI agent runtime. Build, run and ship AI agents in YAML: " +
  "a powerful daemon runtime, a desktop app, a CLI and a Hub of ready-to-run " +
  "agents, with access to frontier models through one gateway.";

const EXCLUDE = [/(^|\/)_/, /(^|\/)archive(\/|$)/, /(^|\/)__/];

// Friendly section labels for the top-level folders.
const SECTION_LABELS = {
  "": "Getting started",
  tutorial: "Tutorials",
  language: "Language",
  concepts: "Concepts",
  howtos: "How-tos",
  reference: "Reference",
  deployment: "Deployment",
  examples: "Examples",
};
const SECTION_ORDER = [
  "",
  "tutorial",
  "language",
  "concepts",
  "howtos",
  "reference",
  "deployment",
  "examples",
];

function walk(dir, rel = "") {
  const out = [];
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const r = rel ? `${rel}/${e.name}` : e.name;
    if (e.isDirectory()) {
      if (EXCLUDE.some((re) => re.test(`${r}/`))) continue;
      out.push(...walk(join(dir, e.name), r));
    } else if (/\.mdx?$/.test(e.name)) {
      if (EXCLUDE.some((re) => re.test(r))) continue;
      out.push(r);
    }
  }
  return out;
}

function parse(relPath) {
  const raw = readFileSync(join(DOCS, relPath), "utf-8");
  let body = raw;
  const fm = {};
  if (raw.startsWith("---")) {
    const end = raw.indexOf("\n---", 3);
    if (end !== -1) {
      const block = raw.slice(3, end);
      body = raw.slice(end + 4).replace(/^\s+/, "");
      for (const line of block.split("\n")) {
        const m = line.match(/^([a-z_]+):\s*(.+)$/i);
        if (m) fm[m[1].toLowerCase()] = m[2].trim().replace(/^["']|["']$/g, "");
      }
    }
  }
  let title = fm.title;
  if (!title) {
    const h1 = body.match(/^#\s+(.+)$/m);
    title = h1 ? h1[1].trim() : relPath.replace(/\.mdx?$/, "");
  }
  return { fm, body, title, description: fm.description || "" };
}

function urlFor(relPath, fm) {
  if (fm.slug && fm.slug.startsWith("/")) {
    return BASE + fm.slug.replace(/\/$/, "");
  }
  const segs = relPath
    .replace(/\.mdx?$/, "")
    .split("/")
    .map((s) => s.replace(/^\d+[-_]/, ""));
  if (segs[segs.length - 1] === "index") segs.pop();
  const path = segs.join("/");
  return path ? `${BASE}/${path}` : BASE;
}

function firstSentence(text) {
  // strip md, take the first real paragraph's first sentence
  const clean = text
    .replace(/```[\s\S]*?```/g, "")
    .replace(/^#.*$/gm, "")
    .replace(/!\[[^\]]*\]\([^)]*\)/g, "")
    .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
    .replace(/[*_`>#]/g, "")
    .split(/\n\s*\n/)
    .map((p) => p.trim())
    .find((p) => p.length > 0);
  if (!clean) return "";
  const s = clean.replace(/\s+/g, " ").trim();
  const dot = s.search(/[.!?](\s|$)/);
  return (dot === -1 ? s : s.slice(0, dot + 1)).slice(0, 200);
}

const files = walk(DOCS).sort();
const pages = files.map((rel) => {
  const p = parse(rel);
  return {
    rel,
    url: SITE + urlFor(rel, p.fm),
    title: p.title,
    description: p.description || firstSentence(p.body),
    body: p.body,
    section: rel.includes("/") ? rel.split("/")[0] : "",
  };
});

// ---- llms.txt (index) -----------------------------------------------------
let idx = `# ${SITE_TITLE}\n\n> ${SITE_BLURB}\n`;
const bySection = new Map();
for (const pg of pages) {
  if (!bySection.has(pg.section)) bySection.set(pg.section, []);
  bySection.get(pg.section).push(pg);
}
const sections = [
  ...SECTION_ORDER.filter((s) => bySection.has(s)),
  ...[...bySection.keys()].filter((s) => !SECTION_ORDER.includes(s)),
];
for (const sec of sections) {
  const label = SECTION_LABELS[sec] || sec || "Docs";
  idx += `\n## ${label}\n\n`;
  for (const pg of bySection.get(sec)) {
    const desc = pg.description ? `: ${pg.description}` : "";
    idx += `- [${pg.title}](${pg.url})${desc}\n`;
  }
}
mkdirSync(OUT, { recursive: true });
writeFileSync(join(OUT, "llms.txt"), idx, "utf-8");

// ---- llms-full.txt (full corpus) -----------------------------------------
let full = `# ${SITE_TITLE} documentation\n\n${SITE_BLURB}\n\n`;
full += `> Full text of every documentation page, for LLM context.\n\n`;
for (const pg of pages) {
  full += `\n\n${"=".repeat(72)}\n# ${pg.title}\nSource: ${pg.url}\n${"=".repeat(72)}\n\n`;
  full += pg.body.trim() + "\n";
}
writeFileSync(join(OUT, "llms-full.txt"), full, "utf-8");

console.log(
  `llms.txt: ${pages.length} pages indexed · llms-full.txt: ${(full.length / 1024).toFixed(0)} KB`,
);
