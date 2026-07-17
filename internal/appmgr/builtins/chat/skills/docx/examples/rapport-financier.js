// TEMPLATE — RAPPORT FINANCIER ANNUEL (avancé).
// Tout ce que Word sait faire, au service d'un document financier :
//   · front matter en chiffres romains, corps redémarré en arabes
//   · TOC automatique (Word la remplit à l'ouverture)
//   · en-tête courant + pied "Page X sur Y"
//   · cartes de KPI (tableau sans bordures, gros chiffres)
//   · GRAPHIQUE EN BARRES construit en cellules ombrées — docx n'a pas de
//     graphique natif : on le dessine avec le tableau
//   · comptes : chiffres à droite, sous-totaux, TOTAL en bordure double,
//     variations en couleur, lignes insécables
//   · notes de bas de page pour les notes comptables
//   · annexe en PAYSAGE pour le détail par entité
const { Document, Packer, Paragraph, TextRun, Table, TableRow, TableCell,
        WidthType, AlignmentType, HeadingLevel, ShadingType, PageBreak,
        BorderStyle, Header, Footer, PageNumber, NumberFormat, SectionType,
        PageOrientation, TableOfContents, FootnoteReferenceRun, VerticalAlign,
        TableBorders, LevelFormat, convertMillimetersToTwip } =
  require(process.env.DGT_SKILLS + "/docx/vendor/docx.cjs");
const fs = require("fs");

/* ── Identité : sobre, chiffré, institutionnel ─────────────────── */
const DEEP = "0f172a", ACCENT = "1e3a5f", INK = "1f2937", MUTED = "64748b";
const LIGHT = "f1f5f9", WHITE = "FFFFFF", POS = "065f46", NEG = "991b1b";
const FONT = "Calibri", NUM = "Consolas";
const W = 9026; // A4 utile

/* ── DONNÉES — tout le document en découle ─────────────────────── */
const EX = "2025";
const KPI = [
  ["Chiffre d'affaires", "12 480 k€", "+18,2 %", true],
  ["EBITDA", "2 310 k€", "+24,5 %", true],
  ["Résultat net", "1 145 k€", "+31,0 %", true],
  ["Trésorerie", "4 820 k€", "−6,4 %", false],
];
const CA_TRIM = [["T1", 2650], ["T2", 2980], ["T3", 3210], ["T4", 3640]]; // k€
const COMPTE = [
  ["Chiffre d'affaires", "12 480", "10 560", "+18,2 %", true, false],
  ["Achats consommés", "(3 120)", "(2 890)", "+8,0 %", false, false],
  ["Charges de personnel", "(5 240)", "(4 610)", "+13,7 %", false, false],
  ["Charges externes", "(1 810)", "(1 520)", "+19,1 %", false, false],
  ["EBITDA", "2 310", "1 540", "+50,0 %", true, true],
  ["Dotations aux amortissements", "(640)", "(580)", "+10,3 %", false, false],
  ["Résultat d'exploitation", "1 670", "960", "+74,0 %", true, true],
  ["Résultat financier", "(95)", "(110)", "−13,6 %", false, false],
  ["Impôt sur les sociétés", "(430)", "(275)", "+56,4 %", false, false],
];
const ENTITES = [
  ["Digitorn OÜ", "Estonie", "8 940", "1 620", "18,1 %", "42"],
  ["Digitorn France SAS", "France", "2 610", "480", "18,4 %", "14"],
  ["Digitorn India Pvt", "Inde", "930", "210", "22,6 %", "9"],
];

/* ── Helpers ───────────────────────────────────────────────────── */
const h1 = (t) => new Paragraph({
  heading: HeadingLevel.HEADING_1,
  shading: { type: ShadingType.CLEAR, fill: ACCENT },
  spacing: { before: 400, after: 200 }, indent: { left: 200, right: 200 },
  children: [new TextRun({ text: t, bold: true, size: 30, color: WHITE, font: FONT })],
});
const h2 = (t) => new Paragraph({
  heading: HeadingLevel.HEADING_2, spacing: { before: 280, after: 120 },
  border: { bottom: { color: ACCENT, size: 6, style: BorderStyle.SINGLE, space: 4 } },
  children: [new TextRun({ text: t, bold: true, size: 24, color: ACCENT, font: FONT })],
});
const para = (t) => new Paragraph({
  alignment: AlignmentType.JUSTIFIED, spacing: { before: 70, after: 70, line: 288 },
  children: [new TextRun({ text: t, size: 20, color: INK, font: FONT })],
});
const legend = (t) => new Paragraph({
  spacing: { before: 60, after: 220 },
  children: [new TextRun({ text: t, italics: true, size: 17, color: MUTED, font: FONT })],
});
const P = (t, o = {}) => new Paragraph({
  alignment: o.align,
  spacing: o.spacing,
  children: [new TextRun({ text: String(t), bold: o.bold, size: o.size || 18,
    color: o.color || INK, font: o.mono ? NUM : FONT })],
});
const C = (t, o = {}) => new TableCell({
  width: { size: o.w, type: WidthType.DXA },
  columnSpan: o.colSpan, rowSpan: o.rowSpan, verticalAlign: o.vAlign,
  shading: o.fill ? { type: ShadingType.CLEAR, fill: o.fill } : undefined,
  borders: o.borders,
  margins: o.margins || { top: 50, bottom: 50, left: 90, right: 90 },
  children: Array.isArray(t) ? t : [P(t, o)],
});

/* ── Cartes de KPI : tableau SANS bordures, gros chiffres ───────── */
function kpiCards(items) {
  const w = Math.floor(W / items.length);
  return new Table({
    width: { size: W, type: WidthType.DXA },
    columnWidths: items.map(() => w),
    borders: TableBorders.NONE,
    rows: [new TableRow({
      children: items.map(([label, val, delta, up]) => C([
        P(label, { size: 16, color: MUTED }),
        P(val, { size: 34, bold: true, color: DEEP, spacing: { before: 40, after: 20 } }),
        P((up ? "▲ " : "▼ ") + delta, { size: 17, bold: true, color: up ? POS : NEG }),
      ], { w, fill: LIGHT, margins: { top: 160, bottom: 160, left: 140, right: 140 } })),
    })],
  });
}

/* ── GRAPHIQUE EN BARRES en cellules ombrées ────────────────────
   docx n'a pas de graphique natif. On dessine : chaque barre est une
   cellule dont la LARGEUR est proportionnelle à la valeur. */
function barChart(data, unit) {
  const max = Math.max(...data.map(([, v]) => v));
  const labelW = 900, valueW = 1100, trackW = W - labelW - valueW;
  return new Table({
    width: { size: W, type: WidthType.DXA },
    columnWidths: [labelW, trackW, valueW],
    borders: TableBorders.NONE,
    rows: data.map(([label, v]) => {
      const barW = Math.max(40, Math.round((v / max) * trackW));
      const restW = trackW - barW;
      // la piste = un tableau imbriqué de 2 cellules : la barre + le vide
      const track = new Table({
        width: { size: trackW, type: WidthType.DXA },
        columnWidths: restW > 0 ? [barW, restW] : [trackW],
        borders: TableBorders.NONE,
        rows: [new TableRow({
          children: [
            C("", { w: barW, fill: ACCENT, margins: { top: 60, bottom: 60, left: 0, right: 0 } }),
            ...(restW > 0 ? [C("", { w: restW, margins: { top: 60, bottom: 60, left: 0, right: 0 } })] : []),
          ],
        })],
      });
      return new TableRow({
        children: [
          C(label, { w: labelW, bold: true, vAlign: VerticalAlign.CENTER }),
          new TableCell({ width: { size: trackW, type: WidthType.DXA },
            verticalAlign: VerticalAlign.CENTER, children: [track] }),
          C(`${v.toLocaleString("fr-FR")} ${unit}`, { w: valueW, mono: true,
            align: AlignmentType.RIGHT, vAlign: VerticalAlign.CENTER }),
        ],
      });
    }),
  });
}

/* ── Compte de résultat ─────────────────────────────────────────── */
function compteResultat(rows) {
  const w = [3626, 1600, 1600, 2200];
  const dbl = { top: { style: BorderStyle.DOUBLE, size: 6, color: DEEP } };
  const head = new TableRow({
    tableHeader: true,
    children: [
      C("En k€", { w: w[0], fill: ACCENT, bold: true, color: WHITE }),
      C(EX, { w: w[1], fill: ACCENT, bold: true, color: WHITE, align: AlignmentType.RIGHT }),
      C(String(+EX - 1), { w: w[2], fill: ACCENT, bold: true, color: WHITE, align: AlignmentType.RIGHT }),
      C("Variation", { w: w[3], fill: ACCENT, bold: true, color: WHITE, align: AlignmentType.RIGHT }),
    ],
  });
  const body = rows.map(([lib, a, b, d, bold, sub], i) => new TableRow({
    cantSplit: true,
    children: [
      C(lib, { w: w[0], bold, fill: sub ? LIGHT : (i % 2 ? LIGHT : WHITE), borders: sub ? dbl : undefined }),
      C(a, { w: w[1], bold, mono: true, align: AlignmentType.RIGHT, fill: sub ? LIGHT : (i % 2 ? LIGHT : WHITE), borders: sub ? dbl : undefined }),
      C(b, { w: w[2], mono: true, align: AlignmentType.RIGHT, color: MUTED, fill: sub ? LIGHT : (i % 2 ? LIGHT : WHITE), borders: sub ? dbl : undefined }),
      C(d, { w: w[3], bold, mono: true, align: AlignmentType.RIGHT, color: d.startsWith("−") ? NEG : POS, fill: sub ? LIGHT : (i % 2 ? LIGHT : WHITE), borders: sub ? dbl : undefined }),
    ],
  }));
  const total = new TableRow({
    children: [
      C("RÉSULTAT NET", { w: w[0], bold: true, borders: dbl, fill: WHITE }),
      C("1 145", { w: w[1], bold: true, mono: true, align: AlignmentType.RIGHT, borders: dbl, fill: WHITE }),
      C("875", { w: w[2], mono: true, align: AlignmentType.RIGHT, color: MUTED, borders: dbl, fill: WHITE }),
      C("+31,0 %", { w: w[3], bold: true, mono: true, align: AlignmentType.RIGHT, color: POS, borders: dbl, fill: WHITE }),
    ],
  });
  return new Table({ width: { size: W, type: WidthType.DXA }, columnWidths: w, rows: [head, ...body, total] });
}

/* ── Document ───────────────────────────────────────────────────── */
const doc = new Document({
  features: { updateFields: true }, // Word remplit la TOC à l'ouverture
  footnotes: {
    1: { children: [para("Les comptes sont établis selon le référentiel applicable et n'ont pas fait l'objet d'un audit externe à la date du présent rapport.")] },
    2: { children: [para("L'EBITDA n'est pas un agrégat normé ; il est présenté à titre indicatif et défini comme le résultat d'exploitation avant dotations aux amortissements.")] },
    3: { children: [para("La variation de trésorerie intègre l'acquisition d'immobilisations incorporelles sur l'exercice.")] },
  },
  sections: [
    /* ── Front matter : i, ii, iii ── */
    {
      properties: { page: {
        size: { width: 11906, height: 16838 },
        margin: { top: convertMillimetersToTwip(25), bottom: convertMillimetersToTwip(20),
                  left: convertMillimetersToTwip(25), right: convertMillimetersToTwip(25) },
        pageNumbers: { start: 1, formatType: NumberFormat.LOWER_ROMAN } } },
      footers: { default: new Footer({ children: [new Paragraph({
        alignment: AlignmentType.CENTER,
        children: [new TextRun({ children: [PageNumber.CURRENT], size: 16, color: MUTED, font: FONT })],
      })] }) },
      children: [
        new Paragraph({ text: "", spacing: { before: 1800 } }),
        new Paragraph({ alignment: AlignmentType.CENTER,
          children: [new TextRun({ text: "RAPPORT FINANCIER", bold: true, size: 52, color: ACCENT, font: FONT })] }),
        new Paragraph({ alignment: AlignmentType.CENTER, spacing: { before: 100 },
          children: [new TextRun({ text: `Exercice clos le 31 décembre ${EX}`, size: 28, color: DEEP, font: FONT })] }),
        new Paragraph({ alignment: AlignmentType.CENTER, spacing: { before: 400 },
          children: [new TextRun({ text: "Nom de la société · Forme juridique · Capital social", size: 20, color: MUTED, font: FONT })] }),
        new Paragraph({ alignment: AlignmentType.CENTER, spacing: { before: 60 },
          children: [new TextRun({ text: "Document non audité · Diffusion restreinte", size: 18, color: MUTED, italics: true, font: FONT })] }),
        new Paragraph({ children: [new PageBreak()] }),
        new Paragraph({ spacing: { after: 200 },
          children: [new TextRun({ text: "Sommaire", bold: true, size: 30, color: ACCENT, font: FONT })] }),
        new TableOfContents("Sommaire", { hyperlink: true, headingStyleRange: "1-2" }),
      ],
    },
    /* ── Corps : 1, 2, 3 ── */
    {
      properties: { type: SectionType.NEXT_PAGE,
        page: { size: { width: 11906, height: 16838 },
          margin: { top: convertMillimetersToTwip(25), bottom: convertMillimetersToTwip(20),
                    left: convertMillimetersToTwip(25), right: convertMillimetersToTwip(25) },
          pageNumbers: { start: 1, formatType: NumberFormat.DECIMAL } } },
      headers: { default: new Header({ children: [new Paragraph({
        alignment: AlignmentType.RIGHT,
        border: { bottom: { color: "d1d5db", size: 4, style: BorderStyle.SINGLE, space: 4 } },
        children: [new TextRun({ text: `Rapport financier ${EX}`, italics: true, size: 16, color: MUTED, font: FONT })],
      })] }) },
      footers: { default: new Footer({ children: [new Paragraph({
        alignment: AlignmentType.CENTER,
        children: [new TextRun({ children: ["Page ", PageNumber.CURRENT, " sur ", PageNumber.TOTAL_PAGES],
          size: 16, color: MUTED, font: FONT })],
      })] }) },
      children: [
        h1("1. Synthèse de l'exercice"),
        para(`L'exercice ${EX} affiche une croissance de l'activité et une amélioration de la rentabilité opérationnelle. Les indicateurs clés sont présentés ci-dessous.`),
        kpiCards(KPI),
        legend("Indicateurs clés — variation à périmètre constant par rapport à l'exercice précédent."),
        new Paragraph({
          alignment: AlignmentType.JUSTIFIED, spacing: { before: 70, after: 70, line: 288 },
          children: [
            new TextRun({ text: "Les comptes présentés dans ce rapport", size: 20, color: INK, font: FONT }),
            new FootnoteReferenceRun(1),
            new TextRun({ text: " couvrent l'ensemble des entités consolidées. L'EBITDA", size: 20, color: INK, font: FONT }),
            new FootnoteReferenceRun(2),
            new TextRun({ text: " progresse plus vite que le chiffre d'affaires, traduisant un effet de levier opérationnel.", size: 20, color: INK, font: FONT }),
          ],
        }),

        h1("2. Évolution du chiffre d'affaires"),
        h2("2.1 Répartition trimestrielle"),
        para("La progression est régulière sur les quatre trimestres, avec une accélération au second semestre."),
        barChart(CA_TRIM, "k€"),
        legend("Figure 1 — Chiffre d'affaires trimestriel (k€). Graphique construit en cellules proportionnelles."),

        h1("3. Compte de résultat"),
        para(`Présentation comparée ${EX} / ${+EX - 1}, en milliers d'euros. Les montants entre parenthèses sont des charges.`),
        compteResultat(COMPTE),
        legend("Tableau 1 — Compte de résultat simplifié. Les sous-totaux et le résultat net sont soulignés en double."),
        new Paragraph({
          alignment: AlignmentType.JUSTIFIED, spacing: { before: 120, after: 70, line: 288 },
          children: [
            new TextRun({ text: "La trésorerie de clôture", size: 20, color: INK, font: FONT }),
            new FootnoteReferenceRun(3),
            new TextRun({ text: " reste supérieure à un an de charges fixes.", size: 20, color: INK, font: FONT }),
          ],
        }),
      ],
    },
    /* ── Annexe PAYSAGE : détail par entité ── */
    {
      properties: { page: { size: { width: 11906, height: 16838, orientation: PageOrientation.LANDSCAPE } } },
      headers: { default: new Header({ children: [new Paragraph({
        alignment: AlignmentType.RIGHT,
        children: [new TextRun({ text: "Annexe", italics: true, size: 16, color: MUTED, font: FONT })],
      })] }) },
      children: [
        h1("Annexe A — Détail par entité"),
        (() => {
          const w = [2600, 1800, 1900, 1900, 1800, 1500];
          return new Table({
            width: { size: 11500, type: WidthType.DXA }, columnWidths: w,
            rows: [
              new TableRow({ tableHeader: true, children: [
                C("Entité", { w: w[0], fill: ACCENT, bold: true, color: WHITE }),
                C("Pays", { w: w[1], fill: ACCENT, bold: true, color: WHITE }),
                C("CA (k€)", { w: w[2], fill: ACCENT, bold: true, color: WHITE, align: AlignmentType.RIGHT }),
                C("EBITDA (k€)", { w: w[3], fill: ACCENT, bold: true, color: WHITE, align: AlignmentType.RIGHT }),
                C("Marge", { w: w[4], fill: ACCENT, bold: true, color: WHITE, align: AlignmentType.RIGHT }),
                C("Effectif", { w: w[5], fill: ACCENT, bold: true, color: WHITE, align: AlignmentType.RIGHT }),
              ] }),
              ...ENTITES.map((r, i) => new TableRow({ cantSplit: true, children: r.map((c, j) =>
                C(c, { w: w[j], mono: j >= 2, align: j >= 2 ? AlignmentType.RIGHT : undefined,
                       fill: i % 2 ? LIGHT : WHITE, bold: j === 0 })) })),
              new TableRow({ children: [
                C("TOTAL CONSOLIDÉ", { w: w[0], bold: true, borders: { top: { style: BorderStyle.DOUBLE, size: 6, color: DEEP } } }),
                C("—", { w: w[1], borders: { top: { style: BorderStyle.DOUBLE, size: 6, color: DEEP } } }),
                C("12 480", { w: w[2], bold: true, mono: true, align: AlignmentType.RIGHT, borders: { top: { style: BorderStyle.DOUBLE, size: 6, color: DEEP } } }),
                C("2 310", { w: w[3], bold: true, mono: true, align: AlignmentType.RIGHT, borders: { top: { style: BorderStyle.DOUBLE, size: 6, color: DEEP } } }),
                C("18,5 %", { w: w[4], bold: true, mono: true, align: AlignmentType.RIGHT, borders: { top: { style: BorderStyle.DOUBLE, size: 6, color: DEEP } } }),
                C("65", { w: w[5], bold: true, mono: true, align: AlignmentType.RIGHT, borders: { top: { style: BorderStyle.DOUBLE, size: 6, color: DEEP } } }),
              ] }),
            ],
          });
        })(),
        legend("Tableau 2 — Contribution par entité. Lisible grâce à l'orientation paysage de cette section."),
      ],
    },
  ],
});

Packer.toBuffer(doc).then((b) => {
  fs.writeFileSync("attachments/rapport-financier.docx", b);
  console.log("OK -> attachments/rapport-financier.docx");
});
