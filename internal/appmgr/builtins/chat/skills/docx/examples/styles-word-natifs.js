// EXEMPLE 2 — VRAIS STYLES WORD NOMMÉS, ultra-précis.
// La leçon : au lieu de styler chaque paragraphe à la main, on déclare des
// STYLES DE DOCUMENT (comme dans Word : "Titre 1", "Citation"…). Le corps ne
// porte alors que style: "NomDuStyle" — et l'utilisateur peut ensuite modifier
// UN style dans Word et TOUT le document suit. C'est le niveau pro.
// Techniques : styles par défaut du document, paragraphStyles (basedOn / next /
// quickFormat / outlineLevel pour la TOC), characterStyles (styles de texte
// inline), marges au millimètre (convertMillimetersToTwip), tabulations à
// points de conduite (PositionalTab), keepNext/keepLines (un titre ne reste
// jamais orphelin en bas de page).
const { Document, Packer, Paragraph, TextRun, AlignmentType, BorderStyle,
        ShadingType, convertMillimetersToTwip, PositionalTab,
        PositionalTabAlignment, PositionalTabRelativeTo, PositionalTabLeader } =
  require(process.env.DGT_SKILLS + "/docx/vendor/docx.cjs");
const fs = require("fs");

const ACCENT = "7c2d12", INK = "292524", MUTED = "78716c"; // palette terre — prouve qu'on n'est pas prisonnier d'un thème

const doc = new Document({
  styles: {
    // Style par défaut de TOUT le document (police, taille, interligne).
    default: {
      document: { run: { font: "Georgia", size: 21, color: INK },
                  paragraph: { spacing: { line: 300, before: 60, after: 60 } } },
    },
    paragraphStyles: [
      { id: "TitrePrincipal", name: "Titre Principal", basedOn: "Normal", next: "CorpsTexte",
        quickFormat: true,
        run: { font: "Georgia", size: 38, bold: true, color: "FFFFFF" },
        paragraph: { spacing: { before: 480, after: 240 }, outlineLevel: 0,
          keepNext: true, // jamais un titre seul en bas de page
          indent: { left: 200, right: 200 },
          shading: { type: ShadingType.CLEAR, fill: ACCENT } } }, // bandeau plein — même en style nommé
      { id: "SousTitre", name: "Sous Titre", basedOn: "Normal", next: "CorpsTexte",
        quickFormat: true,
        run: { font: "Georgia", size: 26, bold: true, color: INK },
        paragraph: { spacing: { before: 280, after: 120 }, outlineLevel: 1, keepNext: true } },
      { id: "CorpsTexte", name: "Corps de texte", basedOn: "Normal", next: "CorpsTexte",
        quickFormat: true,
        paragraph: { alignment: AlignmentType.JUSTIFIED,
          spacing: { line: 320, before: 80, after: 80 } } },
      { id: "Citation", name: "Citation", basedOn: "CorpsTexte", next: "CorpsTexte",
        quickFormat: true,
        run: { italics: true, size: 20, color: MUTED },
        paragraph: { indent: { left: 720, right: 720 }, spacing: { before: 160, after: 160 },
          border: { left: { color: ACCENT, size: 16, style: BorderStyle.SINGLE, space: 12 } } } },
      { id: "EncadreCle", name: "Encadre Cle", basedOn: "CorpsTexte", next: "CorpsTexte",
        quickFormat: true,
        run: { bold: true, size: 20, color: "14532d" },
        paragraph: { indent: { left: 240, right: 240 }, spacing: { before: 140, after: 140 },
          shading: { type: ShadingType.CLEAR, fill: "dcfce7" },
          keepLines: true } }, // l'encadré ne se coupe jamais entre deux pages
      { id: "Legende", name: "Legende", basedOn: "Normal", next: "CorpsTexte",
        run: { italics: true, size: 17, color: MUTED },
        paragraph: { spacing: { before: 60, after: 200 } } },
    ],
    // Styles de CARACTÈRE : s'appliquent à un TextRun, pas au paragraphe.
    characterStyles: [
      { id: "Emphase", name: "Emphase", basedOn: "DefaultParagraphFont",
        run: { bold: true, color: ACCENT } },
      { id: "CodeInline", name: "Code Inline", basedOn: "DefaultParagraphFont",
        run: { font: "Consolas", size: 19, color: "1e40af" } },
    ],
  },
  sections: [{
    properties: { page: {
      size: { width: 11906, height: 16838 },
      // marges au MILLIMÈTRE près — précision typographique réelle
      margin: {
        top: convertMillimetersToTwip(25), bottom: convertMillimetersToTwip(20),
        left: convertMillimetersToTwip(30), right: convertMillimetersToTwip(25),
      },
    } },
    children: [
      // Le corps n'utilise QUE des noms de styles — zéro style inline.
      new Paragraph({ style: "TitrePrincipal", text: "Titre principal du document" }),
      new Paragraph({ style: "CorpsTexte", children: [
        new TextRun("Paragraphe de corps avec un "),
        new TextRun({ text: "terme mis en emphase", style: "Emphase" }),
        new TextRun(" et un identifiant technique "),
        new TextRun({ text: "config.valeur_precise", style: "CodeInline" }),
        new TextRun(" au fil du texte — deux styles de caractère nommés."),
      ] }),
      new Paragraph({ style: "CorpsTexte", text: "Paragraphe de corps justifié, régi entièrement par le style « CorpsTexte » : police, interligne, espacements. Modifier le style dans Word rethème tout le document d'un coup." }),

      new Paragraph({ style: "SousTitre", text: "Une sous-section" }),
      new Paragraph({ style: "CorpsTexte", text: "Paragraphe de corps." }),
      new Paragraph({ style: "Citation", text: "« Une citation longue, en retrait des deux côtés, avec une barre latérale — tout vient du style nommé Citation. »" }),
      new Paragraph({ style: "Legende", text: "Légende discrète sous la citation." }),

      new Paragraph({ style: "SousTitre", text: "Encadré et protection de mise en page" }),
      new Paragraph({ style: "EncadreCle", text: "Message clé : keepLines garantit que cet encadré ne sera jamais coupé entre deux pages ; keepNext garantit qu'un titre n'est jamais orphelin." }),

      new Paragraph({ style: "SousTitre", text: "Tabulations à points de conduite" }),
      new Paragraph({ style: "CorpsTexte", text: "Le motif « libellé …… valeur » (sommaires, bordereaux, signatures) s'obtient avec PositionalTab — JAMAIS avec des points tapés à la main :" }),
      ...["Première entrée du sommaire", "Deuxième entrée", "Troisième entrée"].map((t, i) =>
        new Paragraph({ style: "CorpsTexte", children: [
          new TextRun(t),
          new TextRun({ children: [ new PositionalTab({
            alignment: PositionalTabAlignment.RIGHT,
            relativeTo: PositionalTabRelativeTo.MARGIN,
            leader: PositionalTabLeader.DOT,
          }), `${12 + i * 9}` ] }),
        ] })),
      new Paragraph({ style: "Legende", text: "Points de conduite générés par le moteur de tabulation Word — s'ajustent seuls à la largeur." }),
    ],
  }],
});

Packer.toBuffer(doc).then((b) => {
  fs.writeFileSync("attachments/styles.docx", b);
  console.log("OK -> attachments/styles.docx");
});
