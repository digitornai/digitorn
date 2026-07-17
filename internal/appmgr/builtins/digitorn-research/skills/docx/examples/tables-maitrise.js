// EXEMPLE 3 — MAÎTRISE DES TABLEAUX.
// Techniques : cellules fusionnées en colonne (columnSpan) et en ligne (rowSpan),
// ligne d'en-tête RÉPÉTÉE à chaque page (tableHeader sur un tableau de 50 lignes
// généré par boucle), alignement vertical (VerticalAlign), colonnes numériques
// alignées à droite, lignes insécables (cantSplit), bordures sur mesure (total
// souligné double), tableau financier, et SECTION PAYSAGE pour une matrice de
// 9 colonnes trop large pour le portrait.
const { Document, Packer, Paragraph, TextRun, Table, TableRow, TableCell,
        WidthType, AlignmentType, ShadingType, BorderStyle, VerticalAlign,
        PageOrientation, PageBreak } =
  require(process.env.DGT_SKILLS + "/docx/vendor/docx.cjs");
const fs = require("fs");

const ACCENT = "1e3a5f", INK = "1f2937", MUTED = "6b7280", LIGHT = "eff6ff", WHITE = "FFFFFF";
const FONT = "Calibri";

// Bandeau plein blanc-sur-accent — la signature visuelle des sections.
const h1 = (t) => new Paragraph({
  shading: { type: ShadingType.CLEAR, fill: ACCENT },
  spacing: { before: 360, after: 180 },
  indent: { left: 200, right: 200 },
  children: [new TextRun({ text: t, bold: true, size: 28, color: WHITE, font: FONT })],
});
const legend = (t) => new Paragraph({
  spacing: { before: 60, after: 240 },
  children: [new TextRun({ text: t, italics: true, size: 17, color: MUTED, font: FONT })],
});
const P = (t, o = {}) => new Paragraph({
  alignment: o.align, children: [new TextRun({ text: t, bold: o.bold, size: o.size || 18,
    color: o.color || INK, font: FONT })],
});
// Cellule générique : largeur DXA obligatoire + options fines.
function C(t, o = {}) {
  return new TableCell({
    width: { size: o.w, type: WidthType.DXA },
    columnSpan: o.colSpan, rowSpan: o.rowSpan,
    verticalAlign: o.vAlign,
    shading: o.fill ? { type: ShadingType.CLEAR, fill: o.fill } : undefined,
    borders: o.borders,
    margins: { top: 50, bottom: 50, left: 90, right: 90 },
    children: [P(t, o)],
  });
}
const head = (t, w, o = {}) => C(t, { ...o, w, fill: ACCENT, bold: true, color: WHITE, align: o.align });

const doc = new Document({
  sections: [
    // ────────────────────────── SECTION PORTRAIT ──────────────────────────
    {
      properties: { page: { size: { width: 11906, height: 16838 } } },
      children: [
        h1("1. Cellules fusionnées (columnSpan + rowSpan)"),
        new Table({
          width: { size: 9026, type: WidthType.DXA }, columnWidths: [2200, 2276, 2275, 2275],
          rows: [
            // En-tête à 2 niveaux : une cellule chapeau fusionnée sur 3 colonnes
            new TableRow({ tableHeader: true, children: [
              head("Groupe", 2200, { rowSpan: 2, vAlign: VerticalAlign.CENTER }),
              head("Trimestres", 6826, { colSpan: 3, align: AlignmentType.CENTER }),
            ] }),
            new TableRow({ tableHeader: true, children: [
              head("T1", 2276, { align: AlignmentType.CENTER }),
              head("T2", 2275, { align: AlignmentType.CENTER }),
              head("T3", 2275, { align: AlignmentType.CENTER }),
            ] }),
            // rowSpan : la 1re cellule couvre 3 lignes — les lignes suivantes l'omettent
            new TableRow({ children: [
              C("Groupe A", { w: 2200, rowSpan: 3, vAlign: VerticalAlign.CENTER, bold: true }),
              C("Cellule", { w: 2276 }), C("Cellule", { w: 2275 }), C("Cellule", { w: 2275 }),
            ] }),
            new TableRow({ children: [
              C("Cellule", { w: 2276, fill: LIGHT }), C("Cellule", { w: 2275, fill: LIGHT }), C("Cellule", { w: 2275, fill: LIGHT }),
            ] }),
            new TableRow({ children: [
              C("Cellule", { w: 2276 }), C("Cellule", { w: 2275 }), C("Cellule", { w: 2275 }),
            ] }),
          ],
        }),
        legend("Tableau 1 — Chapeau fusionné sur 3 colonnes, groupe fusionné sur 3 lignes, centrage vertical."),

        h1("2. Tableau financier — nombres à droite, total souligné double"),
        new Table({
          width: { size: 9026, type: WidthType.DXA }, columnWidths: [4526, 2250, 2250],
          rows: [
            new TableRow({ tableHeader: true, children: [
              head("Poste", 4526), head("Budget (€)", 2250, { align: AlignmentType.RIGHT }),
              head("Réel (€)", 2250, { align: AlignmentType.RIGHT }),
            ] }),
            ...[["Poste de dépense A", "1 200 000", "1 184 300"],
                ["Poste de dépense B", "640 000", "702 150"],
                ["Poste de dépense C", "310 000", "298 400"]].map(([a, b, c], i) =>
              new TableRow({ cantSplit: true, children: [
                C(a, { w: 4526, fill: i % 2 ? LIGHT : WHITE }),
                C(b, { w: 2250, align: AlignmentType.RIGHT, fill: i % 2 ? LIGHT : WHITE }),
                C(c, { w: 2250, align: AlignmentType.RIGHT, fill: i % 2 ? LIGHT : WHITE }),
              ] })),
            // Ligne de total : bordure haute DOUBLE — le code comptable classique
            new TableRow({ children: [
              C("TOTAL", { w: 4526, bold: true,
                borders: { top: { style: BorderStyle.DOUBLE, size: 6, color: INK } } }),
              C("2 150 000", { w: 2250, bold: true, align: AlignmentType.RIGHT,
                borders: { top: { style: BorderStyle.DOUBLE, size: 6, color: INK } } }),
              C("2 184 850", { w: 2250, bold: true, align: AlignmentType.RIGHT,
                borders: { top: { style: BorderStyle.DOUBLE, size: 6, color: INK } } }),
            ] }),
          ],
        }),
        legend("Tableau 2 — Chiffres alignés à droite, lignes insécables (cantSplit), total en bordure double."),

        h1("3. Grand tableau multi-pages — en-tête répété"),
        P("50 lignes générées par boucle : l'en-tête bleu se RÉPÈTE automatiquement en haut de chaque page (tableHeader: true).", { size: 20 }),
        new Table({
          width: { size: 9026, type: WidthType.DXA }, columnWidths: [1200, 4426, 1700, 1700],
          rows: [
            new TableRow({ tableHeader: true, children: [
              head("N°", 1200), head("Désignation", 4426),
              head("Quantité", 1700, { align: AlignmentType.RIGHT }),
              head("Montant", 1700, { align: AlignmentType.RIGHT }),
            ] }),
            ...Array.from({ length: 50 }, (_, i) => new TableRow({ cantSplit: true, children: [
              C(String(i + 1), { w: 1200, fill: i % 2 ? LIGHT : WHITE }),
              C("Désignation de la ligne", { w: 4426, fill: i % 2 ? LIGHT : WHITE }),
              C(String((i + 3) * 7), { w: 1700, align: AlignmentType.RIGHT, fill: i % 2 ? LIGHT : WHITE }),
              C(`${(i + 1) * 125},00`, { w: 1700, align: AlignmentType.RIGHT, fill: i % 2 ? LIGHT : WHITE }),
            ] })),
          ],
        }),
        legend("Tableau 3 — Regarde les pages suivantes : l'en-tête est réapparu tout seul."),
      ],
    },
    // ────────────────────────── SECTION PAYSAGE ───────────────────────────
    // Un tableau de 9 colonnes ne tient pas en portrait : on bascule CETTE
    // section seule en paysage (dimensions portrait + orientation LANDSCAPE).
    {
      properties: { page: { size: { width: 11906, height: 16838, orientation: PageOrientation.LANDSCAPE } } },
      children: [
        h1("4. Matrice large — section paysage dédiée"),
        new Table({
          width: { size: 13900, type: WidthType.DXA },
          columnWidths: [2300, 1450, 1450, 1450, 1450, 1450, 1450, 1450, 1450],
          rows: [
            new TableRow({ tableHeader: true, children: [
              head("Critère", 2300),
              ...["Opt. A", "Opt. B", "Opt. C", "Opt. D", "Opt. E", "Opt. F", "Opt. G", "Opt. H"]
                .map((h) => head(h, 1450, { align: AlignmentType.CENTER })),
            ] }),
            ...Array.from({ length: 8 }, (_, r) => new TableRow({ children: [
              C("Critère d'évaluation", { w: 2300, bold: true, fill: r % 2 ? LIGHT : WHITE }),
              ...Array.from({ length: 8 }, () =>
                C(r % 3 === 0 ? "✅" : r % 3 === 1 ? "Partiel" : "❌",
                  { w: 1450, align: AlignmentType.CENTER, fill: r % 2 ? LIGHT : WHITE })),
            ] })),
          ],
        }),
        legend("Tableau 4 — 9 colonnes lisibles grâce à l'orientation paysage de cette section uniquement."),
      ],
    },
  ],
});

Packer.toBuffer(doc).then((b) => {
  fs.writeFileSync("attachments/tables.docx", b);
  console.log("OK -> attachments/tables.docx");
});
