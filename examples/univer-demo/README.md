# Univer Demo — FY2026 Financial Model

Preuve de concept : classeur **multi-feuilles**, **50+ formules**, références croisées, format conditionnel, filtres, validation — le même format JSON qu'un agent digitorn pourrait écrire.

## Lancer

```bash
cd examples/univer-demo
bun install
bun run dev
# → http://localhost:5174/
```

## Où regarder (checklist)

| Feuille | Ce qu'elle prouve |
|---------|-------------------|
| **Dashboard** | KPIs live, `IF()`, variance rouge/vert (format conditionnel), échelle de couleurs sur EBITDA trimestriel |
| **Sales** | 5 produits × 12 mois, `SUM`, YoY %, ligne TOTAL, **auto-filtre** (flèches ▼), **data bars**, validation liste (col. P) |
| **P&L** | Compte de résultat trimestriel lié à Sales + Assumptions, marges %, EBITDA, impôts, résultat net |
| **Assumptions** | Paramètres centralisés (COGS, OpEx, tax) — modifier B2 recalcule tout le modèle |
| **Scenarios** | Best/worst case, sensibilité COGS, seuil de rentabilité |

## Test « puissance » en 30 secondes

1. Onglet **Assumptions** → change `COGS ratio` (B2) de `35%` à `45%`
2. Onglet **Dashboard** → la marge et l'EBITDA se recalculent
3. Onglet **Sales** → clique une flèche de filtre, trie par YoY
4. Sélectionne une plage de chiffres → barre de stats en bas (Somme, Moyenne)
5. Modifie un chiffre mensuel (ex. SaaS Enterprise, Jan) → toute la chaîne Sales → P&L → Dashboard se met à jour

## Architecture (agent-ready)

```
workbook-data.js    → IWorkbookData JSON (source de vérité)
workbook-enhance.js → API Facade (filtres, CF, validation)
univer-setup.js     → bootstrap Univer + presets OSS
```

Un agent digitorn n'aurait qu'à écrire/mettre à jour `workbook-data.js` (ou `workbook.snapshot.json`) — pas besoin de `.xlsx`.

## Fichiers clés

- [`src/workbook-data.js`](src/workbook-data.js) — modèle financier complet
- [`src/workbook-enhance.js`](src/workbook-enhance.js) — enrichissements post-chargement
