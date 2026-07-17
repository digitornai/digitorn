
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
        children: [new TextRun({ text: "Cellule", bold: true, size: 36, color: BLUE })],
        alignment: AlignmentType.CENTER,
        spacing: { before: 0, after: 100 }
      }),
      new Paragraph({
        children: [new TextRun({ text: "Cellule", size: 22, color: GRAY })],
        alignment: AlignmentType.CENTER,
        spacing: { before: 0, after: 60 }
      }),
      new Paragraph({
        children: [new TextRun({ text: "Cellule", size: 20, color: GRAY })],
        alignment: AlignmentType.CENTER,
        spacing: { before: 0, after: 600 }
      }),
      note("Aparté en gris italique."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 1 — VISION
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("TITRE DE SECTION"),
      spacer(),
 
      h2("Headline"),
      para("Paragraphe de corps."),
      spacer(),
 
      h2("Tagline"),
      para("Paragraphe de corps."),
      spacer(),
 
      h2("Sous-titre"),
      highlight("Message clé mis en avant."),
      spacer(),
 
      h2("Supporting context"),
      para("Paragraphe de corps."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 2 — PROBLEM
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("TITRE DE SECTION"),
      spacer(),
 
      h2("Core insight"),
      para("Paragraphe de corps."),
      spacer(),
 
      h2("Sous-titre"),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 3 — SOLUTION
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("TITRE DE SECTION"),
      spacer(),
 
      h2("What Digitorn is"),
      para("Paragraphe de corps."),
      spacer(),
 
      h2("Sous-titre"),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h2("Sous-titre"),
      para("Paragraphe de corps."),
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
      note("Aparté en gris italique."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 4 — PRODUCT / TECH MOAT
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("TITRE DE SECTION"),
      spacer(),
 
      h2("Sous-titre"),
      highlight("Message clé mis en avant."),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Component", "Status", "Detail"],
        [
          ["YAML grammar v1", "✅ Live", "Cellule"],
          ["23 built-in modules", "✅ Live", "Cellule"],
          ["Cellule", "✅ Live", "Cellule"],
          ["3-layer security", "✅ Live", "Cellule"],
          ["Dynamic modes", "✅ Live", "Cellule"],
          ["Multi-agent native", "✅ Live", "Cellule"],
          ["14+ LLM providers", "✅ Live", "Cellule"],
          ["PyPI package", "✅ Live", "Cellule"],
          ["Go runtime (new)", "✅ In progress", "Cellule"],
        ],
        [3000, 1500, 4500]
      ),
      spacer(),
 
      h2("Sous-titre"),
      spacer(),
 
      h3("1. Language lock-in"),
      para("Paragraphe de corps."),
 
      h3("2. Module catalogue"),
      para("Paragraphe de corps."),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
 
      spacer(),
      h2("Competitive Matrix"),
      makeTable(
        ["Capability", "Digitorn", "LangChain", "Claude SDK", "OpenAI SDK", "Docker cagent", "NVIDIA OpenShell"],
        [
          ["Cellule", "✅", "❌", "❌", "❌", "Partial", "❌"],
          ["Cellule", "✅", "✅", "❌", "❌", "✅", "Partial"],
          ["Cellule", "✅", "Partial", "❌", "❌", "✅", "✅"],
          ["Cellule", "✅", "❌", "❌", "❌", "Container", "Container"],
          ["Cellule", "✅", "❌", "❌", "❌", "❌", "❌"],
          ["Cellule", "✅", "❌", "❌", "❌", "❌", "❌"],
          ["Native MCP support", "✅", "✅", "✅", "✅", "✅", "✅"],
          ["Cellule", "✅", "❌", "Limited", "GPTs (closed)", "Hub (generic)", "❌"],
          ["Cellule", "✅", "❌", "❌", "❌", "❌", "❌"],
          ["Cellule", "✅", "❌", "❌", "❌", "❌", "❌"],
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
 
      h2("Sous-titre"),
      spacer(),
 
      h3("Sous-sous-titre"),
      makeTable(
        ["Signal", "2024", "2025", "Q1 2026"],
        [
          ["Cellule", "<5%", "~15%", "Cellule"],
          ["Cellule", "33%", "—", "80%"],
          ["Cellule", "—", "23%", "—"],
          ["Cellule", "<1M", "~40M", "97M+"],
          ["Cursor ARR", "~$0", "$1B", "$2B"],
          ["Lovable ARR", "—", "$100M", "$400M"],
        ],
        [3000, 1800, 1800, 2400]
      ),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h2("Sous-titre"),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      spacer(),
 
      h2("Sous-titre"),
      para("Paragraphe de corps."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 6 — SCREENSHOTS / ARCHITECTURE (instructions)
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("TITRE DE SECTION"),
      spacer(),
 
      h2("Note to Tirth"),
      note("Aparté en gris italique."),
      spacer(),
 
      h2("Sous-titre"),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      spacer(),
 
      h2("Sous-titre"),
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      h3("Sous-sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h2("Sous-titre"),
      para("Paragraphe de corps."),
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
      note("Aparté en gris italique."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 7 — REVENUE MODEL
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("TITRE DE SECTION"),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Plan", "Price", "Credits/month", "Target user", "Key features"],
        [
          ["Free", "$0/mo", "200 credits", "Discovery, testing", "Cellule"],
          ["Starter", "$9/mo", "10,000 credits", "Cellule", "Cellule"],
          ["Pro", "$20/mo", "21,000 credits", "Cellule", "Cellule"],
          ["Max", "$200/mo", "125,000 credits", "Cellule", "Cellule"],
          ["Credit add-ons", "Variable", "Pay-as-you-go", "Any tier", "Cellule"],
        ],
        [1200, 1000, 1400, 2000, 3400]
      ),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Tier", "Price", "Target", "Key features"],
        [
          ["Teams", "€39/user/month", "5-50 seat teams", "Cellule"],
          ["Enterprise", "€2,000+/month base", "Cellule", "Cellule"],
          ["Enterprise+", "Custom", "Cellule", "Cellule"],
        ],
        [1500, 2000, 2200, 3300]
      ),
      spacer(),
 
      h2("Sous-titre"),
      para("Paragraphe de corps."),
      spacer(),
 
      h2("Sous-titre"),
      highlight("Message clé mis en avant."),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Scenario", "Without routing", "With routing", "With BYOK"],
        [
          ["Cellule", "~€420/month", "~€168/month (-60%)", "Cellule"],
          ["Cellule", "Loss-making", "Manageable in pool", "Pure platform margin"],
          ["Gross margin target", "Negative", "55-65%", "85%+"],
        ],
        [3000, 2000, 2000, 2000]
      ),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 8 — UNIT ECONOMICS
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("TITRE DE SECTION"),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Cost item", "Per user / month", "Notes"],
        [
          ["Cellule", "~€4-6", "Cellule"],
          ["Cellule", "~€0.50", "Cellule"],
          ["Stripe fees", "~€0.60", "Cellule"],
          ["Total COGS", "~€5-7", ""],
          ["Cellule", "~65-75%", "Cellule"],
        ],
        [3000, 2000, 4000]
      ),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Cost item", "Per contract / year", "Notes"],
        [
          ["Infrastructure", "€0", "Cellule"],
          ["LLM tokens", "€0", "Cellule"],
          ["Cellule", "~€3,000/year", "Cellule"],
          ["Gross margin", "~92%", "Cellule"],
        ],
        [3000, 2000, 4000]
      ),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Revenue stream", "Year 1", "Year 2", "Year 3"],
        [
          ["Cellule", "€120K", "€1.2M", "€5.4M"],
          ["Teams (5-50 seats)", "€30K", "€300K", "€1.5M"],
          ["Enterprise contracts", "€60K", "€720K", "€2.64M"],
          ["Cellule", "€0", "€0", "€300K"],
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
          ["Cellule", "€750K", "€1.65M", "€3.2M"],
          ["Cellule", "€120K", "€480K", "€1.5M"],
          ["Cellule", "€60K", "€180K", "€400K"],
          ["Cellule", "€50K", "€90K", "€150K"],
          ["Cellule", "€1.02M", "€2.49M", "€5.43M"],
        ],
        [3000, 1800, 1800, 2400]
      ),
      spacer(),
 
      h2("Sous-titre"),
      para("Paragraphe de corps."),
      para("Paragraphe de corps."),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 9 — TEAM
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("SLIDE 9 — The Team"),
      spacer(),
 
      h2("Sous-titre"),
      para("Paragraphe de corps."),
      spacer(60),
      makeTable(
        ["Background", "Detail"],
        [
          ["Education", "Cellule"],
          ["AI Researcher", "Cellule"],
          ["AI Research Engineer", "Cellule"],
          ["Data Scientist", "Cellule"],
          ["Research", "Cellule"],
          ["Languages", "Cellule"],
          ["Links", "Cellule"],
        ],
        [2000, 7000]
      ),
      spacer(),
 
      h2("Sous-titre"),
      para("Paragraphe de corps."),
      spacer(60),
      makeTable(
        ["Background", "Detail"],
        [
          ["Education", "Cellule"],
          ["Business Development", "Cellule"],
          ["COO", "Cellule"],
          ["Certifications", "Cellule"],
          ["Languages", "Cellule"],
          ["Location", "Cellule"],
        ],
        [2000, 7000]
      ),
      spacer(),
 
      h2("Why this team"),
      para("Paragraphe de corps."),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Role", "Month", "Profile"],
        [
          ["Cellule", "M1-M3", "Cellule"],
          ["Cellule", "M2-M4", "Cellule"],
          ["Cellule", "M3-M6", "Cellule"],
          ["Cellule", "M6-M9", "Cellule"],
          ["Cellule", "M9-M12", "Cellule"],
          ["Cellule", "M15-M18", "Cellule"],
        ],
        [3000, 1500, 4500]
      ),
      spacer(200),
 
      // ─────────────────────────────────────────────
      // SLIDE 10 — THE ASK
      // ─────────────────────────────────────────────
      pageBreak(),
      h1("TITRE DE SECTION"),
      spacer(),
 
      h2("This round"),
      highlight("Message clé mis en avant."),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Allocation", "Amount", "%", "Purpose"],
        [
          ["Cellule", "€1,200K", "48%", "Cellule"],
          ["Cellule", "€300K", "12%", "Cellule"],
          ["Cellule", "€400K", "16%", "Cellule"],
          ["Cellule", "€250K", "10%", "Cellule"],
          ["Cellule", "€150K", "6%", "Cellule"],
          ["Cellule", "€100K", "4%", "Cellule"],
          ["Cellule", "€100K", "4%", "Cellule"],
          ["Total", "€2,500K", "100%", ""],
        ],
        [2500, 1200, 800, 4500]
      ),
      spacer(),
 
      h2("Sous-titre"),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      spacer(),
 
      h2("Sous-titre"),
      bullet("€3M+ ARR"),
      bullet("Point de liste."),
      bullet("Point de liste."),
      bullet("Point de liste."),
      spacer(),
 
      h2("Sous-titre"),
      makeTable(
        ["Company", "Round", "Valuation", "ARR at time"],
        [
          ["Letta (MemGPT)", "Seed (Sep 2024)", "$70M", "Pre-revenue"],
          ["Cellule", "Series A (Oct 2024)", "$70-100M", "Early"],
          ["LlamaIndex", "Series A (Mar 2025)", "~$93M", "Early"],
          ["CrewAI", "Series A (Oct 2024)", "~$60M+", "Early"],
          ["LangChain", "Series B (Oct 2025)", "$1.25B", "~$20M ARR"],
          ["Temporal", "Secondary (Oct 2025)", "$2.5B", "Growth stage"],
          ["Digitorn (target)", "Seed 2026", "€8-15M pre-money", "Cellule"],
        ],
        [2000, 1800, 2000, 3200]
      ),
      spacer(200),
 
      // FOOTER
      spacer(300),
      new Paragraph({
        children: [new TextRun({ text: "Cellule", size: 16, color: GRAY })],
        alignment: AlignmentType.CENTER
      }),
    ]
  }]
});
 
Packer.toBuffer(doc).then(buffer => {
  fs.writeFileSync("attachments/exemple.docx", buffer);
  console.log("✅ Document créé : exemple.docx");
});
 