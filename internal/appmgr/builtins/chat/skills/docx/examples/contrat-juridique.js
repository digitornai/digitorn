// EXEMPLE 4 — CONTRAT JURIDIQUE.
// Techniques : numérotation AUTOMATIQUE multi-niveaux (Article 1 — / 1.1 / (a))
// gérée par Word (insérer une clause renumérote tout), articles générés par
// boucle depuis les données, bloc « ENTRE LES SOUSSIGNÉS », définitions,
// tableau de signatures SANS bordures, lignes de signature en points de
// conduite (PositionalTab), pied de page « Page X sur Y » + zone paraphes,
// keepNext pour que l'intitulé d'un article ne soit jamais séparé de son corps.
const { Document, Packer, Paragraph, TextRun, Table, TableRow, TableCell,
        WidthType, AlignmentType, ShadingType, BorderStyle, LevelFormat,
        Footer, PageNumber, TableBorders, PositionalTab, PositionalTabAlignment,
        PositionalTabRelativeTo, PositionalTabLeader, PageBreak } =
  require(process.env.DGT_SKILLS + "/docx/vendor/docx.cjs");
const fs = require("fs");

const INK = "1f2937", MUTED = "6b7280";
const FONT = "Times New Roman"; // registre juridique classique

const para = (t, o = {}) => new Paragraph({
  alignment: AlignmentType.JUSTIFIED,
  spacing: { before: 80, after: 80, line: 300 },
  indent: o.indent,
  children: [new TextRun({ text: t, size: 21, color: INK, font: FONT, bold: o.bold, italics: o.italics })],
});

// Article / clause / sous-clause : TROIS niveaux d'une même numérotation Word.
const article = (t) => new Paragraph({
  numbering: { reference: "articles", level: 0 },
  keepNext: true, // l'intitulé reste collé à son premier alinéa
  spacing: { before: 320, after: 120 },
  children: [new TextRun({ text: t, bold: true, size: 23, color: INK, font: FONT })],
});
const clause = (t) => new Paragraph({
  numbering: { reference: "articles", level: 1 },
  alignment: AlignmentType.JUSTIFIED,
  spacing: { before: 80, after: 80, line: 300 },
  children: [new TextRun({ text: t, size: 21, color: INK, font: FONT })],
});
const sousClause = (t) => new Paragraph({
  numbering: { reference: "articles", level: 2 },
  alignment: AlignmentType.JUSTIFIED,
  spacing: { before: 60, after: 60, line: 300 },
  children: [new TextRun({ text: t, size: 21, color: INK, font: FONT })],
});

// Les articles sont des DONNÉES : insérer/retirer un article renumérote tout,
// puisque la numérotation est native Word, pas tapée dans le texte.
const ARTICLES = [
  { titre: "Objet du contrat", clauses: 2, sous: 2 },
  { titre: "Durée", clauses: 2, sous: 0 },
  { titre: "Obligations du prestataire", clauses: 3, sous: 3 },
  { titre: "Obligations du client", clauses: 2, sous: 2 },
  { titre: "Conditions financières", clauses: 3, sous: 2 },
  { titre: "Confidentialité", clauses: 2, sous: 0 },
  { titre: "Propriété intellectuelle", clauses: 2, sous: 2 },
  { titre: "Responsabilité", clauses: 3, sous: 0 },
  { titre: "Résiliation", clauses: 2, sous: 3 },
  { titre: "Droit applicable et juridiction", clauses: 2, sous: 0 },
];
const corpsArticles = ARTICLES.flatMap((a) => [
  article(a.titre.toUpperCase()),
  ...Array.from({ length: a.clauses }, (_, c) => [
    clause("Texte de la clause, rédigé au registre contractuel, formant un alinéa complet du présent article."),
    ...(c === 0 ? Array.from({ length: a.sous }, () =>
      sousClause("texte de la sous-clause énumérée en lettres ;")) : []),
  ]).flat(),
]);

// Ligne de signature en points de conduite — jamais de "____" tapés à la main.
const ligneSign = (libelle) => new Paragraph({
  spacing: { before: 200 },
  children: [
    new TextRun({ text: libelle + " ", size: 21, font: FONT }),
    new TextRun({ children: [new PositionalTab({
      alignment: PositionalTabAlignment.RIGHT,
      relativeTo: PositionalTabRelativeTo.MARGIN,
      leader: PositionalTabLeader.DOT,
    }), " "], size: 21, font: FONT }),
  ],
});

const doc = new Document({
  numbering: {
    config: [{
      reference: "articles",
      levels: [
        { level: 0, format: LevelFormat.DECIMAL, text: "Article %1 —", alignment: AlignmentType.LEFT },
        { level: 1, format: LevelFormat.DECIMAL, text: "%1.%2.", alignment: AlignmentType.LEFT,
          style: { paragraph: { indent: { left: 460, hanging: 460 } } } },
        { level: 2, format: LevelFormat.LOWER_LETTER, text: "(%3)", alignment: AlignmentType.LEFT,
          style: { paragraph: { indent: { left: 920, hanging: 400 } } } },
      ],
    }],
  },
  sections: [{
    properties: { page: { size: { width: 11906, height: 16838 } } },
    footers: { default: new Footer({ children: [new Paragraph({
      alignment: AlignmentType.CENTER,
      border: { top: { color: "d1d5db", size: 4, style: BorderStyle.SINGLE, space: 4 } },
      children: [new TextRun({
        children: ["Page ", PageNumber.CURRENT, " sur ", PageNumber.TOTAL_PAGES, "        Paraphes :"],
        size: 16, color: MUTED, font: FONT })],
    })] }) },
    children: [
      // ── Titre ──
      new Paragraph({ alignment: AlignmentType.CENTER, spacing: { before: 400, after: 100 },
        children: [new TextRun({ text: "CONTRAT DE PRESTATION DE SERVICES", bold: true, size: 32, font: FONT })] }),
      new Paragraph({ alignment: AlignmentType.CENTER, spacing: { after: 400 },
        children: [new TextRun({ text: "Référence : XXXX-0000", size: 19, color: MUTED, font: FONT })] }),

      // ── Parties ──
      para("ENTRE LES SOUSSIGNÉS :", { bold: true }),
      para("La société Dénomination Sociale, société par actions simplifiée au capital de 00 000 euros, immatriculée au RCS de Ville sous le numéro 000 000 000, dont le siège social est situé Adresse Complète, représentée par Prénom Nom en qualité de Fonction,"),
      para("ci-après désignée « le Prestataire »,", { italics: true, indent: { left: 400 } }),
      para("D'UNE PART,", { bold: true }),
      para("ET :", { bold: true }),
      para("La société Dénomination Sociale, société à responsabilité limitée au capital de 00 000 euros, immatriculée au RCS de Ville sous le numéro 000 000 000, dont le siège social est situé Adresse Complète, représentée par Prénom Nom en qualité de Fonction,"),
      para("ci-après désignée « le Client »,", { italics: true, indent: { left: 400 } }),
      para("D'AUTRE PART,", { bold: true }),

      // ── Préambule ──
      new Paragraph({ spacing: { before: 240, after: 120 },
        children: [new TextRun({ text: "IL A PRÉALABLEMENT ÉTÉ EXPOSÉ CE QUI SUIT :", bold: true, size: 21, font: FONT })] }),
      para("Attendu que le premier considérant expose le contexte de la relation contractuelle ;"),
      para("Attendu que le second considérant expose l'intention commune des parties ;"),
      para("IL A ÉTÉ CONVENU CE QUI SUIT :", { bold: true }),

      // ── Définitions ──
      new Paragraph({ spacing: { before: 240, after: 100 },
        children: [new TextRun({ text: "DÉFINITIONS", bold: true, size: 23, font: FONT })] }),
      ...["« Services »", "« Livrables »", "« Données »"].map((terme) =>
        new Paragraph({ alignment: AlignmentType.JUSTIFIED, spacing: { before: 60, after: 60 },
          indent: { left: 400, hanging: 400 },
          children: [
            new TextRun({ text: terme + " : ", bold: true, size: 21, font: FONT }),
            new TextRun({ text: "définition du terme au sens du présent contrat, opposable entre les parties.", size: 21, font: FONT, color: INK }),
          ] })),

      // ── Corps : les 10 articles auto-numérotés ──
      ...corpsArticles,

      // ── Signatures : tableau invisible 2 colonnes ──
      new Paragraph({ children: [new PageBreak()] }),
      para("Fait à Ville, le JJ/MM/AAAA, en deux exemplaires originaux.", { bold: true }),
      new Table({
        width: { size: 9026, type: WidthType.DXA }, columnWidths: [4513, 4513],
        borders: TableBorders.NONE, // structure sans trait visible
        rows: [new TableRow({ children: ["Pour le Prestataire", "Pour le Client"].map((titre) =>
          new TableCell({
            width: { size: 4513, type: WidthType.DXA },
            margins: { top: 100, bottom: 100, left: 90, right: 340 },
            children: [
              new Paragraph({ children: [new TextRun({ text: titre, bold: true, size: 21, font: FONT })] }),
              ligneSign("Nom :"), ligneSign("Qualité :"), ligneSign("Date :"), ligneSign("Signature :"),
            ],
          })) })],
      }),
    ],
  }],
});

Packer.toBuffer(doc).then((b) => {
  fs.writeFileSync("attachments/contrat.docx", b);
  console.log("OK -> attachments/contrat.docx");
});
