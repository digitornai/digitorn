// EXEMPLE 5 — THÈSE / MÉMOIRE ACADÉMIQUE.
// Techniques : NOTES DE BAS DE PAGE natives Word (FootnoteReferenceRun), front
// matter en chiffres romains puis corps en chiffres arabes (redémarré à 1),
// résumé/abstract, légendes de figures et tableaux numérotées en séquence par
// compteur JS, liste des tableaux à points de conduite, bibliographie en
// retrait suspendu (hanging indent), annexe en paysage.
const { Document, Packer, Paragraph, TextRun, Table, TableRow, TableCell,
        WidthType, AlignmentType, HeadingLevel, ShadingType, BorderStyle,
        Footer, PageNumber, NumberFormat, SectionType, PageOrientation,
        PageBreak, FootnoteReferenceRun, PositionalTab, PositionalTabAlignment,
        PositionalTabRelativeTo, PositionalTabLeader } =
  require(process.env.DGT_SKILLS + "/docx/vendor/docx.cjs");
const fs = require("fs");

const ACCENT = "1e3a8a", INK = "1f2937", MUTED = "6b7280", LIGHT = "eff6ff", WHITE = "FFFFFF";
const FONT = "Cambria";

// Bandeau plein blanc-sur-accent pour les chapitres.
const h1 = (t) => new Paragraph({
  heading: HeadingLevel.HEADING_1,
  shading: { type: ShadingType.CLEAR, fill: ACCENT },
  spacing: { before: 400, after: 200 }, indent: { left: 200, right: 200 },
  children: [new TextRun({ text: t, bold: true, size: 30, color: WHITE, font: FONT })],
});
const h2 = (t) => new Paragraph({
  heading: HeadingLevel.HEADING_2, spacing: { before: 260, after: 110 },
  border: { bottom: { color: ACCENT, size: 6, style: BorderStyle.SINGLE, space: 4 } },
  children: [new TextRun({ text: t, bold: true, size: 24, color: ACCENT, font: FONT })],
});
const para = (t) => new Paragraph({
  alignment: AlignmentType.JUSTIFIED, spacing: { before: 70, after: 70, line: 320 },
  children: [new TextRun({ text: t, size: 21, color: INK, font: FONT })],
});

// Légendes numérotées EN SÉQUENCE — le compteur vit dans le script, pas dans le
// texte : impossible de sauter ou dupliquer un numéro.
let numFig = 0, numTab = 0;
const figCaption = (t) => new Paragraph({
  alignment: AlignmentType.CENTER, spacing: { before: 60, after: 220 },
  children: [new TextRun({ text: `Figure ${++numFig} — ${t}`, italics: true, size: 18, color: MUTED, font: FONT })],
});
const tabCaption = (t) => new Paragraph({
  spacing: { before: 60, after: 220 },
  children: [new TextRun({ text: `Tableau ${++numTab} — ${t}`, italics: true, size: 18, color: MUTED, font: FONT })],
});
const formule = (lines) => lines.map((l, i) => new Paragraph({
  spacing: { before: i === 0 ? 100 : 0, after: i === lines.length - 1 ? 100 : 0 },
  indent: { left: 600 },
  shading: { type: ShadingType.CLEAR, fill: "f1f5f9" },
  children: [new TextRun({ text: l || " ", font: "Consolas", size: 19, color: INK })],
}));
const tbl = (headers, rows, widths) => new Table({
  width: { size: widths.reduce((a, b) => a + b, 0), type: WidthType.DXA }, columnWidths: widths,
  rows: [
    new TableRow({ tableHeader: true, children: headers.map((h, i) => new TableCell({
      width: { size: widths[i], type: WidthType.DXA },
      shading: { type: ShadingType.CLEAR, fill: ACCENT },
      margins: { top: 50, bottom: 50, left: 90, right: 90 },
      children: [new Paragraph({ children: [new TextRun({ text: h, bold: true, color: WHITE, size: 18, font: FONT })] })],
    })) }),
    ...rows.map((r, idx) => new TableRow({ children: r.map((c, i) => new TableCell({
      width: { size: widths[i], type: WidthType.DXA },
      shading: { type: ShadingType.CLEAR, fill: idx % 2 ? LIGHT : WHITE },
      margins: { top: 45, bottom: 45, left: 90, right: 90 },
      children: [new Paragraph({ children: [new TextRun({ text: c, size: 18, color: INK, font: FONT })] })],
    })) })),
  ],
});
// Entrée de bibliographie : retrait suspendu (2e ligne alignée sous l'auteur).
const refBiblio = (t) => new Paragraph({
  alignment: AlignmentType.JUSTIFIED, spacing: { before: 60, after: 60 },
  indent: { left: 500, hanging: 500 },
  children: [new TextRun({ text: t, size: 20, color: INK, font: FONT })],
});

const doc = new Document({
  // NOTES DE BAS DE PAGE : déclarées ici, appelées dans le texte par leur id.
  footnotes: {
    1: { children: [para("Texte de la première note de bas de page, précisant une source.")] },
    2: { children: [para("Deuxième note : commentaire méthodologique détaillé.")] },
    3: { children: [para("Troisième note : référence croisée vers l'annexe.")] },
  },
  sections: [
    // ── Front matter : pages i, ii, iii… ───────────────────────────────────
    {
      properties: { page: { size: { width: 11906, height: 16838 },
        pageNumbers: { start: 1, formatType: NumberFormat.LOWER_ROMAN } } },
      footers: { default: new Footer({ children: [new Paragraph({
        alignment: AlignmentType.CENTER,
        children: [new TextRun({ children: [PageNumber.CURRENT], size: 16, color: MUTED, font: FONT })],
      })] }) },
      children: [
        new Paragraph({ text: "", spacing: { before: 1600 } }),
        new Paragraph({ alignment: AlignmentType.CENTER,
          children: [new TextRun({ text: "Titre de la thèse ou du mémoire", bold: true, size: 44, color: ACCENT, font: FONT })] }),
        new Paragraph({ alignment: AlignmentType.CENTER, spacing: { before: 300 },
          children: [new TextRun({ text: "Prénom NOM", size: 26, color: INK, font: FONT })] }),
        new Paragraph({ alignment: AlignmentType.CENTER, spacing: { before: 200 },
          children: [new TextRun({ text: "Établissement · École doctorale · Année", size: 20, color: MUTED, font: FONT })] }),
        new Paragraph({ children: [new PageBreak()] }),

        h2("Résumé"),
        para("Résumé du travail en français, une dizaine de lignes."),
        h2("Abstract"),
        para("Same summary in English, about ten lines."),
        new Paragraph({ children: [new PageBreak()] }),

        // Liste des tableaux : libellé …… page (points de conduite Word)
        h2("Liste des tableaux"),
        ...[1, 2].map((n) => new Paragraph({
          spacing: { before: 60, after: 60 },
          children: [
            new TextRun({ text: `Tableau ${n} — Intitulé du tableau`, size: 20, font: FONT, color: INK }),
            new TextRun({ children: [new PositionalTab({
              alignment: PositionalTabAlignment.RIGHT,
              relativeTo: PositionalTabRelativeTo.MARGIN,
              leader: PositionalTabLeader.DOT,
            }), String(n * 3)], size: 20, font: FONT, color: INK }),
          ],
        })),
      ],
    },
    // ── Corps : pages 1, 2, 3… ─────────────────────────────────────────────
    {
      properties: { type: SectionType.NEXT_PAGE,
        page: { size: { width: 11906, height: 16838 },
          pageNumbers: { start: 1, formatType: NumberFormat.DECIMAL } } },
      footers: { default: new Footer({ children: [new Paragraph({
        alignment: AlignmentType.CENTER,
        children: [new TextRun({ children: [PageNumber.CURRENT], size: 16, color: MUTED, font: FONT })],
      })] }) },
      children: [
        h1("Introduction"),
        // Appel de note : FootnoteReferenceRun(id) DANS le flux du texte.
        new Paragraph({
          alignment: AlignmentType.JUSTIFIED, spacing: { before: 70, after: 70, line: 320 },
          children: [
            new TextRun({ text: "Paragraphe introducteur s'appuyant sur une source primaire", size: 21, color: INK, font: FONT }),
            new FootnoteReferenceRun(1),
            new TextRun({ text: " et posant la problématique générale du travail.", size: 21, color: INK, font: FONT }),
          ],
        }),
        para("Paragraphe de corps développant le contexte."),

        h1("Méthodologie"),
        new Paragraph({
          alignment: AlignmentType.JUSTIFIED, spacing: { before: 70, after: 70, line: 320 },
          children: [
            new TextRun({ text: "Description du protocole expérimental", size: 21, color: INK, font: FONT }),
            new FootnoteReferenceRun(2),
            new TextRun({ text: ", avec la formalisation suivante :", size: 21, color: INK, font: FONT }),
          ],
        }),
        ...formule(["ŷ = f_θ(x),   θ* = argmin_θ  Σᵢ L(yᵢ, f_θ(xᵢ)) + λ·Ω(θ)"]),
        figCaption("Schéma du protocole (emplacement de la figure)."),

        tbl(["Paramètre", "Valeur", "Justification"],
            [["Cellule", "Cellule", "Cellule"], ["Cellule", "Cellule", "Cellule"], ["Cellule", "Cellule", "Cellule"]],
            [2600, 2600, 3826]),
        tabCaption("Paramètres retenus pour l'expérimentation."),

        h1("Résultats"),
        new Paragraph({
          alignment: AlignmentType.JUSTIFIED, spacing: { before: 70, after: 70, line: 320 },
          children: [
            new TextRun({ text: "Présentation des résultats, renvoyant au détail en annexe", size: 21, color: INK, font: FONT }),
            new FootnoteReferenceRun(3),
            new TextRun({ text: ".", size: 21, color: INK, font: FONT }),
          ],
        }),
        tbl(["Configuration", "Métrique A", "Métrique B"],
            [["Cellule", "0,912", "0,874"], ["Cellule", "0,935", "0,901"]],
            [3600, 2713, 2713]),
        tabCaption("Synthèse des résultats (les numéros de tableau se suivent tout seuls)."),

        h1("Bibliographie"),
        refBiblio("NOM, P. (2024). Titre de l'ouvrage de référence. Maison d'édition, Ville, 312 p."),
        refBiblio("NOM, A. et NOM, B. (2023). « Titre de l'article de revue ». Revue de référence, vol. 12, n° 3, p. 45-67."),
        refBiblio("NOM, C. (2025). Titre de la communication. Actes de la conférence internationale, p. 101-118."),
      ],
    },
    // ── Annexe paysage : le grand tableau de données brutes ────────────────
    {
      properties: { page: { size: { width: 11906, height: 16838, orientation: PageOrientation.LANDSCAPE } } },
      children: [
        h1("Annexe A — Données détaillées"),
        tbl(["Run", "Config", "Seed", "Métrique A", "Métrique B", "Métrique C", "Durée", "Statut"],
            Array.from({ length: 12 }, (_, i) =>
              [String(i + 1), "Cellule", String(40 + i), "0,9xx", "0,8xx", "0,7xx", "Cellule", i % 4 ? "OK" : "Rejeté"]),
            [1200, 2200, 1200, 1700, 1700, 1700, 1800, 1900]),
        tabCaption("Résultats bruts complets — lisibles grâce à la section paysage."),
      ],
    },
  ],
});

Packer.toBuffer(doc).then((b) => {
  fs.writeFileSync("attachments/these.docx", b);
  console.log("OK -> attachments/these.docx");
});
