// EXEMPLE 1 — LIVRE / RAPPORT LONG (centaines de pages), généré par BOUCLES.
// La leçon : un document énorme n'est PAS un script énorme — c'est une structure
// de données + des boucles. NB_CHAPITRES contrôle l'échelle (40 ≈ 100 pages,
// 120 ≈ 300 pages). Techniques : sections multiples (couverture + front matter
// en chiffres romains, corps en chiffres arabes redémarrant à 1), table des
// matières AUTOMATIQUE (mise à jour à l'ouverture dans Word), en-têtes/pieds de
// page avec numéros de page, titres numérotés multi-niveaux (Chapitre N / N.M).
const { Document, Packer, Paragraph, TextRun, Table, TableRow, TableCell,
        WidthType, AlignmentType, HeadingLevel, ShadingType, PageBreak,
        LevelFormat, BorderStyle, Header, Footer, PageNumber, NumberFormat,
        TableOfContents, SectionType } =
  require(process.env.DGT_SKILLS + "/docx/vendor/docx.cjs");
const fs = require("fs");

const ACCENT = "1e3a5f", INK = "1f2937", MUTED = "6b7280", LIGHT = "eff6ff", WHITE = "FFFFFF";
const FONT = "Calibri";

// Masse de paragraphe réaliste (structure seule, contenu vide de sens).
const LOREM = "Corps de texte du chapitre, phrase de démonstration qui donne la masse et le rythme d'un vrai paragraphe de livre. ".repeat(6);

// ── L'échelle du document est UNE CONSTANTE ────────────────────────────────
const NB_CHAPITRES = 40; // 40 ≈ 100 pages · 120 ≈ 300 pages
const CHAPITRES = Array.from({ length: NB_CHAPITRES }, (_, i) => ({
  titre: `Titre du chapitre ${i + 1}`,
  sections: Array.from({ length: 4 }, (_, j) => ({
    titre: `Sous-partie ${i + 1}.${j + 1}`,
    nbParas: 3,
  })),
}));

// h1 = BANDEAU PLEIN (texte blanc sur fond accent) — la signature visuelle forte.
function h1(text) {
  return new Paragraph({
    heading: HeadingLevel.HEADING_1,
    numbering: { reference: "chapitres", level: 0 },
    shading: { type: ShadingType.CLEAR, fill: ACCENT },
    spacing: { before: 400, after: 200 },
    indent: { left: 200, right: 200 },
    children: [new TextRun({ text, bold: true, size: 32, color: WHITE, font: FONT })],
  });
}
// h2 = accent + filet bas.
function h2(text) {
  return new Paragraph({
    heading: HeadingLevel.HEADING_2,
    numbering: { reference: "chapitres", level: 1 },
    spacing: { before: 260, after: 110 },
    border: { bottom: { color: ACCENT, size: 6, style: BorderStyle.SINGLE, space: 4 } },
    children: [new TextRun({ text, bold: true, size: 25, color: ACCENT, font: FONT })],
  });
}
function para(text) {
  return new Paragraph({
    spacing: { before: 60, after: 60, line: 276 },
    alignment: AlignmentType.JUSTIFIED,
    children: [new TextRun({ text, size: 20, color: INK, font: FONT })],
  });
}
function key(text) {
  return new Paragraph({
    spacing: { before: 100, after: 100 },
    indent: { left: 200, right: 200 },
    shading: { type: ShadingType.CLEAR, fill: "d1fae5" },
    children: [new TextRun({ text, bold: true, size: 20, color: "065f46", font: FONT })],
  });
}
function tableChapitre() {
  const widths = [3000, 3000, 3026];
  const head = new TableRow({
    tableHeader: true,
    children: ["Élément", "Valeur", "Commentaire"].map((h, i) => new TableCell({
      width: { size: widths[i], type: WidthType.DXA },
      shading: { type: ShadingType.CLEAR, fill: ACCENT },
      children: [new Paragraph({ children: [new TextRun({ text: h, bold: true, color: WHITE, size: 18, font: FONT })] })],
    })),
  });
  const rows = Array.from({ length: 4 }, (_, r) => new TableRow({
    children: widths.map((w) => new TableCell({
      width: { size: w, type: WidthType.DXA },
      shading: { type: ShadingType.CLEAR, fill: r % 2 === 0 ? WHITE : LIGHT },
      children: [new Paragraph({ children: [new TextRun({ text: "Cellule", size: 18, color: INK, font: FONT })] })],
    })),
  }));
  return new Table({ width: { size: 9026, type: WidthType.DXA }, columnWidths: widths, rows: [head, ...rows] });
}

// Un chapitre = un tableau d'éléments, produit par une fonction. C'est la brique
// qui rend les centaines de pages possibles.
function chapitre(chap) {
  const out = [new Paragraph({ children: [new PageBreak()] }), h1(chap.titre)];
  for (const s of chap.sections) {
    out.push(h2(s.titre));
    for (let p = 0; p < s.nbParas; p++) out.push(para(LOREM));
  }
  out.push(tableChapitre());
  out.push(key("Message clé du chapitre, mis en avant."));
  return out;
}

const doc = new Document({
  features: { updateFields: true }, // Word met à jour la TOC à l'ouverture
  numbering: {
    config: [{
      reference: "chapitres",
      levels: [
        // le numéro du bandeau doit être blanc comme le titre
        { level: 0, format: LevelFormat.DECIMAL, text: "Chapitre %1 —", alignment: AlignmentType.LEFT,
          style: { run: { bold: true, color: "FFFFFF" } } },
        { level: 1, format: LevelFormat.DECIMAL, text: "%1.%2", alignment: AlignmentType.LEFT,
          style: { run: { bold: true, color: "1e3a5f" }, paragraph: { indent: { left: 200 } } } },
      ],
    }],
  },
  sections: [
    // ── Section 1 : front matter, numérotée i, ii, iii… ────────────────────
    {
      properties: { page: { size: { width: 11906, height: 16838 },
        pageNumbers: { start: 1, formatType: NumberFormat.LOWER_ROMAN } } },
      footers: { default: new Footer({ children: [new Paragraph({
        alignment: AlignmentType.CENTER,
        children: [new TextRun({ children: [PageNumber.CURRENT], size: 16, color: MUTED, font: FONT })],
      })] }) },
      children: [
        new Paragraph({ text: "", spacing: { before: 2000 } }),
        new Paragraph({ alignment: AlignmentType.CENTER,
          children: [new TextRun({ text: "TITRE DE L'OUVRAGE", bold: true, size: 64, color: ACCENT, font: FONT })] }),
        new Paragraph({ alignment: AlignmentType.CENTER, spacing: { before: 200 },
          children: [new TextRun({ text: "Sous-titre de l'ouvrage", size: 30, color: INK, font: FONT })] }),
        new Paragraph({ alignment: AlignmentType.CENTER, spacing: { before: 400 },
          children: [new TextRun({ text: "Nom de l'auteur · Année", size: 20, color: MUTED, font: FONT })] }),
        new Paragraph({ children: [new PageBreak()] }),
        new Paragraph({ spacing: { after: 200 },
          children: [new TextRun({ text: "Table des matières", bold: true, size: 30, color: ACCENT, font: FONT })] }),
        // TOC AUTOMATIQUE : construite depuis les HeadingLevel 1-2. LibreOffice
        // peut l'afficher vide ; Word la remplit à l'ouverture (updateFields).
        new TableOfContents("Table des matières", { hyperlink: true, headingStyleRange: "1-2" }),
      ],
    },
    // ── Section 2 : corps, numéroté 1, 2, 3… avec en-tête + pied ───────────
    {
      properties: { type: SectionType.NEXT_PAGE,
        page: { size: { width: 11906, height: 16838 },
          pageNumbers: { start: 1, formatType: NumberFormat.DECIMAL } } },
      headers: { default: new Header({ children: [new Paragraph({
        alignment: AlignmentType.RIGHT,
        border: { bottom: { color: "d1d5db", size: 4, style: BorderStyle.SINGLE, space: 4 } },
        children: [new TextRun({ text: "Titre de l'ouvrage", italics: true, size: 16, color: MUTED, font: FONT })],
      })] }) },
      footers: { default: new Footer({ children: [new Paragraph({
        alignment: AlignmentType.CENTER,
        children: [new TextRun({ children: ["— ", PageNumber.CURRENT, " —"], size: 16, color: MUTED, font: FONT })],
      })] }) },
      children: CHAPITRES.flatMap(chapitre),
    },
  ],
});

Packer.toBuffer(doc).then((b) => {
  fs.writeFileSync("attachments/livre.docx", b);
  console.log("OK -> attachments/livre.docx");
});
