
const { Document, Paragraph, Table, TableRow, TableCell, TextRun, HeadingLevel, AlignmentType, WidthType, ShadingType, Packer, PageBreak, NumberingLevel } = require(process.env.DGT_SKILLS + "/docx/vendor/docx.cjs");
const fs = require("fs");
 
const BLUE = "1e3a5f";
const ACCENT = "2563eb";
const LIGHT = "eff6ff";
const WHITE = "FFFFFF";
const GRAY = "6b7280";
const GREEN = "065f46";
const GREEN_BG = "d1fae5";
 
function h1(text) {
  return new Paragraph({
    children: [new TextRun({ text, bold: true, size: 32, color: WHITE })],
    shading: { type: ShadingType.CLEAR, fill: ACCENT },
    spacing: { before: 400, after: 200 },
    indent: { left: 200, right: 200 }
  });
}
 
function h2(text) {
  return new Paragraph({
    children: [new TextRun({ text, bold: true, size: 26, color: ACCENT })],
    spacing: { before: 300, after: 120 },
    border: { bottom: { color: ACCENT, size: 6, style: "single" } }
  });
}
 
function h3(text) {
  return new Paragraph({
    children: [new TextRun({ text, bold: true, size: 22, color: BLUE })],
    spacing: { before: 200, after: 80 }
  });
}
 
function para(text, opts = {}) {
  return new Paragraph({
    children: [new TextRun({ text, size: 20, color: "1f2937", ...opts })],
    spacing: { before: 60, after: 60 }
  });
}
 
function bullet(text) {
  return new Paragraph({
    children: [new TextRun({ text: `• ${text}`, size: 20, color: "1f2937" })],
    spacing: { before: 40, after: 40 },
    indent: { left: 400 }
  });
}
 
function note(text) {
  return new Paragraph({
    children: [new TextRun({ text, italics: true, size: 18, color: GRAY })],
    spacing: { before: 60, after: 60 },
    shading: { type: ShadingType.CLEAR, fill: "f9fafb" },
    indent: { left: 200, right: 200 }
  });
}
 
function spacer(size = 100) {
  return new Paragraph({ text: "", spacing: { before: size, after: size } });
}
 
function highlight(text) {
  return new Paragraph({
    children: [new TextRun({ text, bold: true, size: 20, color: GREEN })],
    shading: { type: ShadingType.CLEAR, fill: GREEN_BG },
    spacing: { before: 80, after: 80 },
    indent: { left: 200, right: 200 }
  });
}
 
function makeTable(headers, rows, colWidths) {
  const totalWidth = colWidths.reduce((a, b) => a + b, 0);
 
  const headerRow = new TableRow({
    children: headers.map((h, i) => new TableCell({
      width: { size: colWidths[i], type: WidthType.DXA },
      shading: { type: ShadingType.CLEAR, fill: ACCENT },
      children: [new Paragraph({
        children: [new TextRun({ text: h, bold: true, color: WHITE, size: 18 })],
        alignment: AlignmentType.CENTER
      })]
    }))
  });
 
  const dataRows = rows.map((row, idx) => new TableRow({
    children: row.map((cell, i) => new TableCell({
      width: { size: colWidths[i], type: WidthType.DXA },
      shading: { type: ShadingType.CLEAR, fill: idx % 2 === 0 ? WHITE : LIGHT },
      children: [new Paragraph({
        children: [new TextRun({ text: cell, size: 18, color: "1f2937" })]
      })]
    }))
  }));
 
  return new Table({
    width: { size: totalWidth, type: WidthType.DXA },
    rows: [headerRow, ...dataRows]
  });
}
 
function pageBreak() {
  return new Paragraph({ children: [new PageBreak()] });
}
 
const doc = new Document({
  sections: [{
    properties: { page: { size: { width: 12240, height: 15840 } } },
    children: [
 
      // COVER
      new Paragraph({
        children: [new TextRun({ text: "DIGITORN", bold: true, size: 72, color: ACCENT })],
        alignment: AlignmentType.CENTER,
        spacing: { before: 600, after: 100 }
      }),
      new Paragraph({
        children: [new TextRun({ text: "Pitch Deck Content Package", bold: true, size: 36, color: BLUE })],
        alignment: AlignmentType.CENTER,
        spacing: { before: 0, after: 100 }
      }),
      new Paragraph({
        children: [new TextRun({ text: "Prepared by Paul Mbathe Mekontchou — Founder & CEO", size: 22, color: GRAY })],
        alignment: AlignmentType.CENTER,
        spacing: { before: 0, after: 60 }
      }),
      new Paragraph({
        children: [new TextRun({ text: "July 2026 · Confidential", size: 20, color: GRAY })],
        alignment: AlignmentType.CENTER,
        spacing: { before: 0, after: 600 }
      }),
      note("This document contains the full content for each pitch deck slide. Tirth will use this to create the investor presentation. All figures, quotes and data points are sourced directly from the Digitorn Seed Business Plan (May 2026)."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 1 — VISION
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 1 — Vision & Cover"),
      spacer(),
 
      h2("Headline"),
      para("The runtime for AI agents."),
      spacer(),
 
      h2("Tagline"),
      para("Declarative. Self-hostable. Open by design."),
      spacer(),
 
      h2("One-sentence formula (We build X for Y to solve Z)"),
      highlight("We build sovereign AI agent infrastructure for any company to deploy, govern and run production agents — without rebuilding the plumbing from scratch or depending on a single cloud vendor."),
      spacer(),
 
      h2("Supporting context"),
      para("Every wave of computing has produced a runtime that absorbed its complexity: the JVM for enterprise software, the browser for the web, iOS and Android for mobile, Docker and Kubernetes for cloud workloads. AI agents are now a category of software in their own right. Digitorn is that runtime."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 2 — PROBLEM
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 2 — The Problem"),
      spacer(),
 
      h2("Core insight"),
      para("Five years of LLM progress has produced exceptional models and exceptional applications — but no neutral substrate to host them. The category needs an open, declarative, vendor-neutral runtime."),
      spacer(),
 
      h2("5 specific pain points"),
      spacer(),
 
      h3("1. Every team rebuilds the same stack from scratch"),
      para("Every AI agent needs the same machinery: an LLM call loop, tool calling, memory, security perimeter, logging, deployment. The vast majority of teams copy snippets from blog posts and glue incompatible libraries. The result is fragile, hard to audit, and expensive to maintain."),
      spacer(),
 
      h3("2. Vendor SDKs lock you into one model"),
      para("Anthropic (Claude Agent SDK, Sept 2025), OpenAI (Agents SDK, March 2025), Google (ADK, April 2025), Microsoft (Agent Framework 1.0, April 2026) — each SDK is optimised for its own models and cloud. A company that wants Claude for reasoning, GPT-4o for vision, and a local Mistral for cost-sensitive bulk tasks hits a dead end on any proprietary SDK."),
      spacer(),
 
      h3("3. Individual users pay 5+ subscriptions for the same plumbing"),
      para("ChatGPT Plus ($20) + Claude Pro ($20) + Cursor ($20-40) + GitHub Copilot ($10) + others = $150+/month. Every tool re-implements the same primitives underneath. From an investor's perspective, this is the classic signal that a category is ready for consolidation: when many vertical apps share the same underlying engine, the engine is the business."),
      spacer(),
 
      h3("4. Regulators now require what nobody is shipping"),
      para("The EU AI Act enters full enforcement on 2 August 2026 — in 3 weeks. Articles 12 (logging), 13 (transparency) and 14 (human oversight) translate directly into runtime properties. Cloud SDKs that hide agent internals behind a remote API cannot satisfy these requirements by themselves. A declarative, auditable, self-hostable runtime can."),
      spacer(),
 
      h3("5. No neutral, open substrate exists"),
      para("Model providers push lock-in. Open-source frameworks are libraries (you still write the loop). End-users pay multiple times for identical plumbing. Regulators want auditable behaviour the current stack cannot easily provide. The gap is real and large."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 3 — SOLUTION
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 3 — The Solution"),
      spacer(),
 
      h2("What Digitorn is"),
      para("Digitorn is a declarative runtime that interprets a YAML file describing an agent application — its brain, tools, memory, security boundaries, triggers — and runs it as a production daemon. Any company goes from zero to a fully operational AI agent in minutes, not months. No engineering team required. Just a config file."),
      spacer(),
 
      h2("Three platform layers"),
      spacer(),
 
      h3("Layer 1 — The Runtime (open-source, free)"),
      para("Source-available under BSL 1.1 (converts to Apache 2.0 after 3 years). Self-hostable, free for individuals and internal corporate use. One command: pip install digitorn. Ships with 23 pre-built modules, connects to 14+ LLM providers."),
      spacer(),
 
      h3("Layer 2 — The Hub (marketplace)"),
      para("A registry of installable agent applications — the App Store for AI agents. Users browse, click install, and the agent runs. Already seeded with 6 high-quality reference applications. Revenue-sharing programme for third-party creators (80% creator, 20% Digitorn)."),
      spacer(),
 
      h3("Layer 3 — Digitorn Cloud (managed, paid)"),
      para("For individuals: one subscription, access to hundreds of agents through a hosted runtime. For enterprises: governed deployment in the customer's VPC or on-premise, with SSO, audit logs, RBAC, compliance documentation, and SLA-backed support."),
      spacer(),
 
      h2("Hello World — 20 lines of YAML"),
      para("The following excerpt is taken directly from docs.digitorn.ai, verified against the v1 schema:"),
      spacer(60),
      new Paragraph({
        children: [new TextRun({
          text:
`app:
  app_id: hello
  name: "Hello Assistant"
 
agents:
  - id: assistant
    role: assistant
    brain:
      provider: anthropic
      model: claude-sonnet-4-6
    system_prompt: |
      You are a helpful assistant. Reply concisely.
 
tools:
  modules:
    memory: {}
  capabilities:
    default_policy: auto`,
          font: "Courier New",
          size: 18,
          color: "1f2937"
        })],
        shading: { type: ShadingType.CLEAR, fill: "f8fafc" },
        spacing: { before: 60, after: 60 },
        indent: { left: 400 }
      }),
      note("This compiles, deploys, and starts answering chat turns. The same 8-block shape scales to multi-agent coordinators with channels, sub-agent fan-out, hooks, scheduled triggers, RAG retrieval, and OS-level sandboxing."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 4 — PRODUCT / TECH MOAT
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 4 — Product & Tech Moat"),
      spacer(),
 
      h2("USP — Unique Selling Proposition"),
      highlight("The only AI agent platform that is simultaneously: declarative (YAML, no orchestration code), vendor-neutral (14+ LLM providers), sovereign (self-hostable, single static binary, runs anywhere), governed (3-layer security), and marketplace-enabled (Hub with installable agents)."),
      spacer(),
 
      h2("MVP — What is already live today"),
      makeTable(
        ["Component", "Status", "Detail"],
        [
          ["YAML grammar v1", "✅ Live", "8 blocks, Pydantic-validated, compile-time errors on typos, frozen stability guarantee"],
          ["23 built-in modules", "✅ Live", "Filesystem, shell, web, RAG, MCP, channels, databases, memory, widgets and more"],
          ["6 reference apps on Hub", "✅ Live", "Coding Assistant, Live Sandbox, Voice Agent, Slack Copilot, Automation Pack, Support Triage"],
          ["3-layer security", "✅ Live", "7 permission gates + behavior engine (14 rules) + OS sandbox (Landlock/seccomp/Seatbelt)"],
          ["Dynamic modes", "✅ Live", "plan/act/custom — switch agent permissions mid-session without restart"],
          ["Multi-agent native", "✅ Live", "Coordinator + specialists, parallel fan-out, sub-agent pools"],
          ["14+ LLM providers", "✅ Live", "OpenAI, Anthropic, Mistral, DeepSeek, Groq, Ollama, vLLM and more"],
          ["PyPI package", "✅ Live", "Signed releases with Sigstore attestations — pip install digitorn"],
          ["Go runtime (new)", "✅ In progress", "34,550 turns/sec single process, zero goroutine leak, fault injection tested"],
        ],
        [3000, 1500, 4500]
      ),
      spacer(),
 
      h2("5 Defensibility Pillars"),
      spacer(),
 
      h3("1. Language lock-in"),
      para("YAML grammar frozen at v1 with a public stability guarantee. Once developers write apps against this schema, those apps are portable across Digitorn versions — but not to other runtimes without rewriting. Switching cost grows with every YAML written."),
 
      h3("2. Module catalogue"),
      para("23 modules covering most of what an agent needs out of the box. A competitor offering a similar declarative model would need to rebuild the same surface — roughly an engineer-year of work for each module done properly."),
 
      h3("3. Security architecture"),
      para("OS-level sandboxing across 3 operating systems (Linux Landlock/seccomp, macOS Seatbelt, Windows Job Objects) is unglamorous, slow work. Most competitors will not do it. This is what makes the Hub safe for enterprise procurement to consider."),
 
      h3("4. Hub network effects"),
      para("Two-sided marketplace: 100 high-quality agents → the next 1,000 follow. The cost of building a competing marketplace from scratch becomes prohibitive once the Hub reaches critical mass."),
 
      h3("5. Documentation contract"),
      para("Every YAML example in the documentation is verified against a live running daemon on every release. If a doc example doesn't work, it's a bug. The operational rigour this represents is high, and the trust it builds compounds over time."),
 
      spacer(),
      h2("Competitive Matrix"),
      makeTable(
        ["Capability", "Digitorn", "LangChain", "Claude SDK", "OpenAI SDK", "Docker cagent", "NVIDIA OpenShell"],
        [
          ["Declarative YAML (no code required)", "✅", "❌", "❌", "❌", "Partial", "❌"],
          ["Vendor-neutral (10+ LLM providers)", "✅", "✅", "❌", "❌", "✅", "Partial"],
          ["Self-hostable / on-premise", "✅", "Partial", "❌", "❌", "✅", "✅"],
          ["OS-level sandbox (Landlock/seccomp)", "✅", "❌", "❌", "❌", "Container", "Container"],
          ["Dynamic permission modes", "✅", "❌", "❌", "❌", "❌", "❌"],
          ["Behavior engine (14 rules)", "✅", "❌", "❌", "❌", "❌", "❌"],
          ["Native MCP support", "✅", "✅", "✅", "✅", "✅", "✅"],
          ["Hub / agent marketplace", "✅", "❌", "Limited", "GPTs (closed)", "Hub (generic)", "❌"],
          ["Mobile / edge / WASM / air-gapped", "✅", "❌", "❌", "❌", "❌", "❌"],
          ["Frozen v1 stability guarantee", "✅", "❌", "❌", "❌", "❌", "❌"],
        ],
        [3000, 1100, 1100, 1100, 1100, 1200, 1400]
      ),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 5 — WHY NOW
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 5 — Why Now?"),
      spacer(),
 
      h2("Three forces converging in 2026"),
      spacer(),
 
      h3("Force 1 — AI agents moved from demos to production (faster than anyone expected)"),
      makeTable(
        ["Signal", "2024", "2025", "Q1 2026"],
        [
          ["Enterprise apps with task-specific agents (Gartner)", "<5%", "~15%", "40% projected by year-end"],
          ["New/updated enterprise apps with embedded agent", "33%", "—", "80%"],
          ["Organisations scaling at least one agentic system (McKinsey)", "—", "23%", "—"],
          ["MCP SDK downloads per month", "<1M", "~40M", "97M+"],
          ["Cursor ARR", "~$0", "$1B", "$2B"],
          ["Lovable ARR", "—", "$100M", "$400M"],
        ],
        [3000, 1800, 1800, 2400]
      ),
      spacer(),
 
      h3("Force 2 — The interoperability standard is set"),
      para("Anthropic published MCP (Model Context Protocol) in November 2024. It now sees 97 million SDK downloads/month and has been adopted by OpenAI, Google, Microsoft, and Salesforce in under twelve months. This is the standard that makes a declarative, vendor-neutral runtime viable at scale — before MCP, every integration was custom. Digitorn was built natively on MCP from day one."),
      spacer(),
 
      h3("Force 3 — The EU AI Act forces the architecture we already built"),
      para("The EU AI Act enters full enforcement on 2 August 2026 — in 3 weeks. High-risk AI systems (banking, healthcare, public sector) must demonstrate auditable behaviour, traceable decisions, and effective human oversight. Articles 12, 13 and 14 translate directly into runtime properties. US cloud-only SDKs fail this test structurally. Digitorn passes it by design — it was the founding architectural decision, not a retrofit."),
      spacer(),
 
      h2("Why wasn't this viable 2 years ago?"),
      bullet("MCP didn't exist (November 2024 publication)"),
      bullet("The agent category wasn't a production reality at scale"),
      bullet("The EU AI Act wasn't in force"),
      bullet("The market validation by Docker, NVIDIA and Genspark hadn't happened"),
      spacer(),
 
      h2("Why will the window close if we wait?"),
      para("Enterprise buyers are making their AI agent infrastructure choices in 2026. These decisions lock in vendor relationships for 3-5 years. The companies chosen now will compound. Those that arrive in 2027 will find locked-in incumbents. The market validation by giants (Docker cagent May 2025, NVIDIA OpenShell March 2026, Genspark $275M raised November 2025) confirms demand — but none of them serve regulated European enterprises. That window is open right now and closing."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 6 — SCREENSHOTS / ARCHITECTURE (instructions)
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 6 — Screenshots, Architecture & Workflows"),
      spacer(),
 
      h2("Note to Tirth"),
      note("Paul will provide the screenshots and visual assets listed below. These need to be captured from app.digitorn.ai and integrated into the deck by Tirth. The YAML example below is ready to use directly."),
      spacer(),
 
      h2("Screenshots needed from app.digitorn.ai"),
      bullet("Landing page / before login — shows the platform value proposition"),
      bullet("Dashboard after login — shows the main interface"),
      bullet("Hub page — list of available agents to install"),
      bullet("Builder interface — creating a new agent"),
      bullet("Agent in action — a live use case example (e.g. Coding Assistant or Research agent)"),
      bullet("YAML editor in Builder — showing a real config being written"),
      spacer(),
 
      h2("Architecture diagram — 3 layers to illustrate"),
      h3("Layer 1 — User interface"),
      para("Web app (app.digitorn.ai) · Mobile app · API / SDK — connects via REST + Socket.IO to the daemon"),
      h3("Layer 2 — Digitorn Runtime (the daemon)"),
      para("YAML parser → AppDefinition → Module bootstrapper → Event loop → AgentContext → LLM provider calls → Tool dispatcher → 3-layer security (Capabilities / Behavior engine / OS sandbox) → Audit log"),
      h3("Layer 3 — Infrastructure / providers"),
      para("14+ LLM providers (OpenAI, Anthropic, Mistral, DeepSeek, Groq, Ollama…) · 23 modules (filesystem, shell, web, RAG, MCP, channels…) · Storage (PostgreSQL, Vector DB) · OS (Linux/macOS/Windows/mobile/WASM)"),
      spacer(),
 
      h2("YAML Configuration Engine — live example"),
      para("This is a real, working agent definition from docs.digitorn.ai:"),
      spacer(60),
      new Paragraph({
        children: [new TextRun({
          text:
`app:
  app_id: research-assistant
  name: "Research Assistant"
 
agents:
  - id: researcher
    role: assistant
    brain:
      provider: anthropic
      model: claude-haiku-4-5
    system_prompt: |
      You are a research assistant. Search the web,
      summarise findings, and cite your sources.
 
tools:
  modules:
    web:
      max_results: 10
    memory: {}
    rag:
      index: research_kb
 
  capabilities:
    default_policy: auto
    rules:
      - action: "filesystem.*"
        policy: deny
      - action: "web.search"
        policy: grant
 
security:
  behavior:
    profile: research
    rules:
      - id: no_pii
        description: "Never output personal data"
        enforcement: block`,
          font: "Courier New",
          size: 17,
          color: "1f2937"
        })],
        shading: { type: ShadingType.CLEAR, fill: "f8fafc" },
        spacing: { before: 60, after: 60 },
        indent: { left: 400 }
      }),
      note("This agent: searches the web, stores findings in a RAG knowledge base, has memory across sessions, is blocked from writing to the filesystem, and enforces a 'no personal data' behavioral rule. All in 40 lines of YAML."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 7 — REVENUE MODEL
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 7 — Revenue Model B2C / B2B"),
      spacer(),
 
      h2("Revenue Stream 1 — B2C Consumer Subscriptions (live at digitorn.ai/pricing)"),
      makeTable(
        ["Plan", "Price", "Credits/month", "Target user", "Key features"],
        [
          ["Free", "$0/mo", "200 credits", "Discovery, testing", "Access to existing agents, 200+ connectors, free models"],
          ["Starter", "$9/mo", "10,000 credits", "Individual developers", "Agent builder, SOTA models (Claude Opus, GPT)"],
          ["Pro", "$20/mo", "21,000 credits", "Power users, small teams", "Unlimited runs, automations, cloud agents, MCP, webhooks"],
          ["Max", "$200/mo", "125,000 credits", "Heavy users, agencies", "Priority compute, extended context, everything"],
          ["Credit add-ons", "Variable", "Pay-as-you-go", "Any tier", "Purchase additional credits at any time"],
        ],
        [1200, 1000, 1400, 2000, 3400]
      ),
      spacer(),
 
      h2("Revenue Stream 2 — B2B Enterprise (contact sales)"),
      makeTable(
        ["Tier", "Price", "Target", "Key features"],
        [
          ["Teams", "€39/user/month", "5-50 seat teams", "SSO, shared workspaces, audit logs, team billing"],
          ["Enterprise", "€2,000+/month base", "100+ seats / regulated", "On-premise or VPC, SLA, dedicated CSM, AI Act compliance docs"],
          ["Enterprise+", "Custom", "Banks, hospitals, defence", "Air-gapped deployment, security audit pack, custom modules"],
        ],
        [1500, 2000, 2200, 3300]
      ),
      spacer(),
 
      h2("Revenue Stream 3 — Hub Marketplace"),
      para("Third-party developers publish paid agents on the Hub. Revenue split: 80% to the creator, 20% to Digitorn. Same model as the App Store, Steam, and Shopify. Scales with the marketplace, not with our headcount. Modelled conservatively starting Year 3."),
      spacer(),
 
      h2("Key insight — Token cost reduction strategy"),
      highlight("Multi-model routing cuts LLM costs by ~60% in production. Premium models (Claude Sonnet, GPT-4o) write the final output. Cheap models (DeepSeek, Groq, Haiku, local Ollama) handle research, drafting, verification (90% of calls). Documented on digitorn.ai/blog."),
      spacer(),
 
      h2("Worst-case heavy user analysis"),
      makeTable(
        ["Scenario", "Without routing", "With routing", "With BYOK"],
        [
          ["Senior dev at full tilt (2M input + 500K output tokens/day)", "~€420/month", "~€168/month (-60%)", "€0 token cost for Digitorn"],
          ["Impact on €20/month subscriber", "Loss-making", "Manageable in pool", "Pure platform margin"],
          ["Gross margin target", "Negative", "55-65%", "85%+"],
        ],
        [3000, 2000, 2000, 2000]
      ),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 8 — UNIT ECONOMICS
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 8 — Business & Operating Model"),
      spacer(),
 
      h2("Unit economics — B2C Consumer (Pro at $20/month)"),
      makeTable(
        ["Cost item", "Per user / month", "Notes"],
        [
          ["LLM tokens (with multi-model routing)", "~€4-6", "Premium model for output only; cheap/local for research"],
          ["Infrastructure (shared, amortised)", "~€0.50", "Runtime daemon, storage, CDN"],
          ["Stripe fees", "~€0.60", "2.9% + €0.30 per transaction"],
          ["Total COGS", "~€5-7", ""],
          ["Gross margin at €20 subscription", "~65-75%", "In line with SaaS infrastructure benchmarks"],
        ],
        [3000, 2000, 4000]
      ),
      spacer(),
 
      h2("Unit economics — B2B Enterprise self-hosted"),
      makeTable(
        ["Cost item", "Per contract / year", "Notes"],
        [
          ["Infrastructure", "€0", "Client hosts everything on their own infra"],
          ["LLM tokens", "€0", "Client uses their own provider keys (BYOK mandatory)"],
          ["Support & compliance docs", "~€3,000/year", "Dedicated CSM, AI Act compliance pack, SLA"],
          ["Gross margin", "~92%", "Highest-margin SaaS model: license + support only"],
        ],
        [3000, 2000, 4000]
      ),
      spacer(),
 
      h2("Financial projections — Base case (from Seed Business Plan)"),
      makeTable(
        ["Revenue stream", "Year 1", "Year 2", "Year 3"],
        [
          ["Consumer Cloud (Pro + Pro+)", "€120K", "€1.2M", "€5.4M"],
          ["Teams (5-50 seats)", "€30K", "€300K", "€1.5M"],
          ["Enterprise contracts", "€60K", "€720K", "€2.64M"],
          ["Hub marketplace fee (20%)", "€0", "€0", "€300K"],
          ["Total revenue", "€210K", "€2.22M", "€9.84M"],
          ["Annualised exit ARR", "~€480K", "~€3.6M", "~€14.4M"],
        ],
        [3000, 1800, 1800, 2400]
      ),
      spacer(),
 
      h2("Cost structure"),
      makeTable(
        ["Cost line", "Year 1", "Year 2", "Year 3"],
        [
          ["Payroll (engineering, sales, ops)", "€750K", "€1.65M", "€3.2M"],
          ["Cloud infrastructure & LLM tokens", "€120K", "€480K", "€1.5M"],
          ["Marketing, content, events", "€60K", "€180K", "€400K"],
          ["Legal, accounting, admin", "€50K", "€90K", "€150K"],
          ["Total operating expenses", "€1.02M", "€2.49M", "€5.43M"],
        ],
        [3000, 1800, 1800, 2400]
      ),
      spacer(),
 
      h2("Corporate architecture advantage"),
      para("Parent company: Digitorn OÜ (Estonia) — EU legal entity, access to European regulated enterprise buyers, AI Act compliance native, GDPR by design, e-Residency incorporated. All international contracts signed through the Estonian entity."),
      para("India operations: highly cost-effective engineering and sales execution via Tirth's base in Gujarat. India B2B SaaS market growing rapidly (Freshworks, Zoho, Postman precedents). DPDP 2023 creates the same regulatory tailwind as AI Act in Europe. Go-to-market via IT services (TCS, Infosys, Wipro) and regulated Indian sectors."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 9 — TEAM
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 9 — The Team"),
      spacer(),
 
      h2("Paul Mbathe Mekontchou — Founder & CEO"),
      para("The technical founder. Owns the runtime, the product, the technical roadmap, and all engineering hires."),
      spacer(60),
      makeTable(
        ["Background", "Detail"],
        [
          ["Education", "Specialized Master's in AI — Télécom Paris (2024-2025) · Engineer's degree in Computer Science — École Polytechnique de Yaoundé (2016-2021)"],
          ["AI Researcher", "DLR (German Aerospace Center) — LLMs applied to anomaly detection in time-series data, aerospace R&D environment (Jan-Jun 2026)"],
          ["AI Research Engineer", "STMicroelectronics, Paris — Unsupervised multimodal model for industrial defect detection (no labeled data); production RAG pipeline for non-conformity reports (Jun 2025-Jan 2026)"],
          ["Data Scientist", "Groupe ALUCAM, Cameroon — ML pipelines in production: +2% efficiency, -5% costs, -3% downtime (Feb 2022-Aug 2024)"],
          ["Research", "DataWill, Luxembourg — Semantic search & RAG research, published as arXiv:2302.10150 (Jan-Nov 2021)"],
          ["Languages", "French (native), English (fluent), German (basic)"],
          ["Links", "digitorn.ai · github.com/mbathe/digitorn-bridge · linkedin.com/in/paul-mbathe-8243ab18b"],
        ],
        [2000, 7000]
      ),
      spacer(),
 
      h2("Tirth Sureshbhai Donda — Co-founder & CCO"),
      para("The commercial founder. Owns go-to-market, enterprise sales, partnerships, fundraising operations, and India operations."),
      spacer(60),
      makeTable(
        ["Background", "Detail"],
        [
          ["Education", "MSc Global Foresight & Technology Management — TH Ingolstadt, Germany (thesis: RegTech in India & Germany, directly relevant to EU AI Act) · BSc Robotics & Automation — Silesian University of Technology, Poland"],
          ["Business Development", "Siemens AG Nuremberg — Grid Software, 16 months. Global sales strategy, Power BI dashboards, ~10% increase in premium product adoption globally"],
          ["COO", "Tirth Gems, India — Operational turnaround: +18% profit margins, launched in-house production, built B2B client network"],
          ["Certifications", "Project Management (PMI) · Corporate Finance (NASBA) · Strategic Planning (PMI) · Corporate Strategy (U. of London) · ERP SAP S/4HANA"],
          ["Languages", "English (bilingual), Hindi (bilingual), Gujarati (native)"],
          ["Location", "Gujarat, India — day-one operational presence for India go-to-market"],
        ],
        [2000, 7000]
      ),
      spacer(),
 
      h2("Why this team"),
      para("The pattern of a technical founder paired with a commercial co-founder is the most common shape of successful seed-stage B2B infrastructure companies: Snowflake, Confluent, HashiCorp, Datadog. The split is clean: Paul builds, Tirth sells. Joint decisions cover only hiring above a certain seniority, financial controls, and strategic positioning."),
      spacer(),
 
      h2("Planned hires (post-seed close)"),
      makeTable(
        ["Role", "Month", "Profile"],
        [
          ["Senior Runtime Engineer", "M1-M3", "Python/Go async, infrastructure or distributed systems background"],
          ["Senior Frontend Engineer", "M2-M4", "React/TypeScript — Hub UI, Builder, Cloud dashboard"],
          ["DevRel / Community Lead", "M3-M6", "Open-source background, content credibility"],
          ["Security & Sandbox Engineer", "M6-M9", "Landlock/seccomp/OS internals experience"],
          ["Enterprise Sales Engineer", "M9-M12", "Technical background, regulated-industry exposure"],
          ["Account Executive (EMEA)", "M15-M18", "Mid-market and large-enterprise infrastructure sales"],
        ],
        [3000, 1500, 4500]
      ),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 10 — THE ASK
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 10 — The Ask & Use of Funds"),
      spacer(),
 
      h2("This round"),
      highlight("Raising: €1.5M to €3M seed round · Target: €2.5M · Pre-money valuation: €8M to €15M · Runway: 18-24 months to Series A"),
      spacer(),
 
      h2("Use of proceeds — €2.5M base case"),
      makeTable(
        ["Allocation", "Amount", "%", "Purpose"],
        [
          ["Engineering (5 hires)", "€1,200K", "48%", "Runtime hardening, Builder UI, Cloud control plane"],
          ["Go-to-market & content", "€300K", "12%", "DevRel, events, paid acquisition, partnerships"],
          ["Sales (2 hires from M9)", "€400K", "16%", "Enterprise design partners → first contracts"],
          ["Infrastructure & LLM costs", "€250K", "10%", "Cloud hosting, model spend, observability"],
          ["Security & compliance", "€150K", "6%", "SOC 2 Type I, AI Act readiness, sandbox audits"],
          ["Legal, accounting, admin", "€100K", "4%", "Standard operating costs"],
          ["Buffer / opportunistic", "€100K", "4%", "Strategic flexibility"],
          ["Total", "€2,500K", "100%", ""],
        ],
        [2500, 1200, 800, 4500]
      ),
      spacer(),
 
      h2("Milestones funded by this round"),
      bullet("Ship runtime v1.5 with multi-tenant Hub, marketplace transactions, and Cloud subscription billing"),
      bullet("Reach 25,000 GitHub stars and 5,000 paying Cloud subscribers"),
      bullet("Close 5+ enterprise design partnerships (France, Germany, Nordics; India: Bangalore-Mumbai-Gujarat axis)"),
      bullet("Convert at least 2 design partners to paying enterprise contracts"),
      bullet("Achieve SOC 2 Type I and document AI Act conformity"),
      bullet("Reach €3M annualised ARR with gross margin above 65%"),
      spacer(),
 
      h2("Series A target metrics (M18-M24)"),
      bullet("€3M+ ARR"),
      bullet("Demonstrated NRR above 110%"),
      bullet("Mature Hub with community-contributed agents"),
      bullet("2+ paying enterprise contracts in regulated industries"),
      spacer(),
 
      h2("Comparable valuations at seed stage"),
      makeTable(
        ["Company", "Round", "Valuation", "ARR at time"],
        [
          ["Letta (MemGPT)", "Seed (Sep 2024)", "$70M", "Pre-revenue"],
          ["Pydantic AI / Logfire", "Series A (Oct 2024)", "$70-100M", "Early"],
          ["LlamaIndex", "Series A (Mar 2025)", "~$93M", "Early"],
          ["CrewAI", "Series A (Oct 2024)", "~$60M+", "Early"],
          ["LangChain", "Series B (Oct 2025)", "$1.25B", "~$20M ARR"],
          ["Temporal", "Secondary (Oct 2025)", "$2.5B", "Growth stage"],
          ["Digitorn (target)", "Seed 2026", "€8-15M pre-money", "Pre-revenue, product live"],
        ],
        [2000, 1800, 2000, 3200]
      ),
      spacer(200),
 
      // FOOTER
      spacer(300),
      new Paragraph({
        children: [new TextRun({ text: "DIGITORN · digitorn.ai · mbathepaul@gmail.com · Confidential · July 2026", size: 16, color: GRAY })],
        alignment: AlignmentType.CENTER
      }),
    ]
  }]
});
 
Packer.toBuffer(doc).then(buffer => {
  fs.writeFileSync("attachments/Digitorn_Pitch_Content_For_Tirth.docx", buffer);
  console.log("✅ Document créé : Digitorn_Pitch_Content_For_Tirth.docx");
});
 