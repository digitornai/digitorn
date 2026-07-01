/** Shared styles referenced by id (Univer style deduplication). */
export const styles = {
  title: { bl: 1, fs: 14, bg: { rgb: '#217346' }, cl: { rgb: '#FFFFFF' } },
  header: { bl: 1, bg: { rgb: '#E2EFDA' } },
  subheader: { bl: 1, bg: { rgb: '#D9D9D9' } },
  currency: { n: { pattern: '$#,##0' } },
  currencyBold: { bl: 1, n: { pattern: '$#,##0' } },
  pct: { n: { pattern: '0.0%' } },
  pctBold: { bl: 1, n: { pattern: '0.0%' } },
  totalRow: { bl: 1, bg: { rgb: '#FFF2CC' }, n: { pattern: '$#,##0' } },
  good: { cl: { rgb: '#217346' }, bl: 1 },
  bad: { cl: { rgb: '#B42318' }, bl: 1 },
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']

function cell(value, styleId) {
  if (styleId) return { v: value, s: styleId }
  return { v: value }
}

function formula(expr, styleId) {
  if (styleId) return { f: expr, s: styleId }
  return { f: expr }
}

function header(value) {
  return { v: value, s: 'header' }
}

function monthHeaders(startCol = 1) {
  const row = { 0: header('Product / Line') }
  MONTHS.forEach((m, i) => {
    row[startCol + i] = header(m)
  })
  row[13] = header('FY Total')
  row[14] = header('YoY %')
  return row
}

function monthColLetter(index) {
  // 1 -> B, 12 -> M
  return String.fromCharCode(66 + index - 1)
}

function buildSalesSheet() {
  const products = [
    { name: 'SaaS Enterprise', base: 85000, growth: 0.025 },
    { name: 'SaaS SMB', base: 42000, growth: 0.03 },
    { name: 'Professional Services', base: 28000, growth: 0.015 },
    { name: 'Marketplace Fees', base: 15000, growth: 0.04 },
    { name: 'Training & Cert.', base: 9000, growth: 0.02 },
  ]

  const cellData = {
    0: { 0: { v: 'FY2026 Sales Pipeline — monthly revenue by product line', s: 'title' } },
    1: monthHeaders(),
  }

  products.forEach((p, i) => {
    const row = i + 2
    const excelRow = row + 1
    cellData[row] = { 0: cell(p.name) }
    let val = p.base
    for (let m = 0; m < 12; m++) {
      cellData[row][m + 1] = { v: Math.round(val), s: 'currency', t: 2 }
      val *= 1 + p.growth
    }
    cellData[row][13] = formula(`=SUM(B${excelRow}:M${excelRow})`, 'currencyBold')
    cellData[row][14] = formula(`=(N${excelRow}/B${excelRow})-1`, 'pct')
  })

  const totalRow = products.length + 2
  const totalExcel = totalRow + 1
  cellData[totalRow] = { 0: { v: 'TOTAL REVENUE', s: 'subheader' } }
  for (let m = 1; m <= 12; m++) {
    const col = monthColLetter(m)
    cellData[totalRow][m] = formula(`=SUM(${col}3:${col}${totalExcel - 1})`, 'totalRow')
  }
  cellData[totalRow][13] = formula(`=SUM(N3:N${totalExcel - 1})`, 'totalRow')
  cellData[totalRow][14] = formula(`=AVERAGE(O3:O${totalExcel - 1})`, 'pctBold')

  return {
    id: 'sales',
    name: 'Sales',
    tabColor: '#217346',
    freeze: { xSplit: 1, ySplit: 2, startRow: 2, startColumn: 1 },
    mergeData: [{ startRow: 0, endRow: 0, startColumn: 0, endColumn: 14 }],
    columnData: {
      0: { w: 180 },
      13: { w: 110 },
      14: { w: 80 },
    },
    cellData,
    rowCount: 120,
    columnCount: 20,
  }
}

function buildAssumptionsSheet() {
  return {
    id: 'assumptions',
    name: 'Assumptions',
    tabColor: '#7F7F7F',
    cellData: {
      0: { 0: header('Parameter'), 1: header('Value'), 2: header('Notes') },
      1: { 0: cell('COGS ratio'), 1: { v: 0.35, s: 'pct', t: 2 }, 2: cell('% of revenue') },
      2: { 0: cell('Monthly OpEx'), 1: { v: 45000, s: 'currency', t: 2 }, 2: cell('Fixed overhead') },
      3: { 0: cell('Tax rate'), 1: { v: 0.25, s: 'pct', t: 2 }, 2: cell('Corporate tax') },
      4: { 0: cell('Revenue target'), 1: { v: 1200000, s: 'currency', t: 2 }, 2: cell('FY2026 goal') },
      5: { 0: cell('Growth target'), 1: { v: 0.15, s: 'pct', t: 2 }, 2: cell('YoY ambition') },
      6: { 0: cell('Best case multiplier'), 1: { v: 1.12, t: 2 }, 2: cell('Scenario planning') },
      7: { 0: cell('Worst case multiplier'), 1: { v: 0.88, t: 2 }, 2: cell('Scenario planning') },
    },
    columnData: { 0: { w: 180 }, 1: { w: 100 }, 2: { w: 220 } },
    rowCount: 40,
    columnCount: 6,
  }
}

function buildPLSheet() {
  const cellData = {
    0: { 0: { v: 'Profit & Loss — FY2026 (linked to Sales + Assumptions)', s: 'title' } },
    1: { 0: header('Line item'), 1: header('Q1'), 2: header('Q2'), 3: header('Q3'), 4: header('Q4'), 5: header('FY Total'), 6: header('% Rev') },
    2: { 0: cell('Revenue'), 1: formula("=SUM('Sales'!B3:D7)/3*3", 'currency'), 2: formula("=SUM('Sales'!E3:G7)/3*3", 'currency'), 3: formula("=SUM('Sales'!H3:J7)/3*3", 'currency'), 4: formula("=SUM('Sales'!K3:M7)/3*3", 'currency'), 5: formula("='Sales'!N8", 'currencyBold') },
    3: { 0: cell('COGS'), 1: formula('=B3*Assumptions!B2', 'currency'), 2: formula('=C3*Assumptions!B2', 'currency'), 3: formula('=D3*Assumptions!B2', 'currency'), 4: formula('=E3*Assumptions!B2', 'currency'), 5: formula('=F3*Assumptions!B2', 'currency') },
    4: { 0: { v: 'Gross profit', s: 'subheader' }, 1: formula('=B3-B4', 'currencyBold'), 2: formula('=C3-C4', 'currencyBold'), 3: formula('=D3-D4', 'currencyBold'), 4: formula('=E3-E4', 'currencyBold'), 5: formula('=F3-F4', 'currencyBold'), 6: formula('=F5/F3', 'pctBold') },
    5: { 0: cell('Operating expenses'), 1: formula('=Assumptions!B3*3', 'currency'), 2: formula('=Assumptions!B3*3', 'currency'), 3: formula('=Assumptions!B3*3', 'currency'), 4: formula('=Assumptions!B3*3', 'currency'), 5: formula('=Assumptions!B3*12', 'currency') },
    6: { 0: { v: 'EBITDA', s: 'subheader' }, 1: formula('=B5-B6', 'currencyBold'), 2: formula('=C5-C6', 'currencyBold'), 3: formula('=D5-D6', 'currencyBold'), 4: formula('=E5-E6', 'currencyBold'), 5: formula('=F5-F6', 'currencyBold'), 6: formula('=F7/F3', 'pctBold') },
    7: { 0: cell('Tax'), 1: formula('=MAX(B7,0)*Assumptions!B4', 'currency'), 2: formula('=MAX(C7,0)*Assumptions!B4', 'currency'), 3: formula('=MAX(D7,0)*Assumptions!B4', 'currency'), 4: formula('=MAX(E7,0)*Assumptions!B4', 'currency'), 5: formula('=MAX(F7,0)*Assumptions!B4', 'currency') },
    8: { 0: { v: 'Net income', s: 'subheader' }, 1: formula('=B7-B8', 'currencyBold'), 2: formula('=C7-C8', 'currencyBold'), 3: formula('=D7-D8', 'currencyBold'), 4: formula('=E7-E8', 'currencyBold'), 5: formula('=F7-F8', 'currencyBold'), 6: formula('=F9/F3', 'pctBold') },
  }

  return {
    id: 'pl',
    name: 'P&L',
    tabColor: '#4472C4',
    freeze: { xSplit: 1, ySplit: 2, startRow: 2, startColumn: 1 },
    mergeData: [{ startRow: 0, endRow: 0, startColumn: 0, endColumn: 6 }],
    columnData: { 0: { w: 180 }, 5: { w: 110 }, 6: { w: 80 } },
    cellData,
    rowCount: 80,
    columnCount: 10,
  }
}

function buildDashboardSheet() {
  return {
    id: 'dashboard',
    name: 'Dashboard',
    tabColor: '#FFC000',
    cellData: {
      0: { 0: { v: 'Executive Dashboard — live KPIs (all formulas, cross-sheet)', s: 'title' } },
      2: { 0: header('KPI'), 1: header('Actual'), 2: header('Target'), 3: header('Variance'), 4: header('Status') },
      3: {
        0: cell('FY Revenue'),
        1: formula("='P&L'!F3", 'currencyBold'),
        2: formula('=Assumptions!B5', 'currency'),
        3: formula('=B4-C4', 'currencyBold'),
        4: formula('=IF(D4>=0,"✓ On track","✗ Below target")'),
      },
      4: {
        0: cell('Gross margin %'),
        1: formula("='P&L'!G5", 'pctBold'),
        2: { v: 0.65, s: 'pct', t: 2 },
        3: formula('=B5-C5', 'pctBold'),
        4: formula('=IF(B5>=C5,"✓ Healthy","Review COGS")'),
      },
      5: {
        0: cell('EBITDA'),
        1: formula("='P&L'!F7", 'currencyBold'),
        2: formula('=Assumptions!B5*0.18', 'currency'),
        3: formula('=B6-C6', 'currencyBold'),
        4: formula('=IF(D6>=0,"✓ On track","✗ Below target")'),
      },
      6: {
        0: cell('Net income'),
        1: formula("='P&L'!F9", 'currencyBold'),
        2: formula('=Assumptions!B5*0.12', 'currency'),
        3: formula('=B7-C7', 'currencyBold'),
        4: formula('=IF(D7>=0,"✓ Profitable","Loss-making")'),
      },
      8: { 0: header('Quarterly EBITDA trend') },
      9: { 0: header('Q1'), 1: header('Q2'), 2: header('Q3'), 3: header('Q4') },
      10: {
        0: formula("='P&L'!B7", 'currencyBold'),
        1: formula("='P&L'!C7", 'currencyBold'),
        2: formula("='P&L'!D7", 'currencyBold'),
        3: formula("='P&L'!E7", 'currencyBold'),
      },
      12: { 0: cell('Formula count in this workbook'), 1: { v: '50+', t: 2 } },
      13: { 0: cell('Cross-sheet references'), 1: { v: 'Sales ↔ P&L ↔ Assumptions ↔ Dashboard', t: 1 } },
    },
    mergeData: [{ startRow: 0, endRow: 0, startColumn: 0, endColumn: 4 }],
    columnData: { 0: { w: 200 }, 1: { w: 120 }, 2: { w: 120 }, 3: { w: 120 }, 4: { w: 160 } },
    rowCount: 60,
    columnCount: 8,
  }
}

function buildScenariosSheet() {
  return {
    id: 'scenarios',
    name: 'Scenarios',
    tabColor: '#7030A0',
    cellData: {
      0: { 0: { v: 'Scenario analysis — IF + multipliers from Assumptions', s: 'title' } },
      2: { 0: header('Scenario'), 1: header('Revenue'), 2: header('EBITDA'), 3: header('Net income'), 4: header('vs Base') },
      3: {
        0: cell('Base'),
        1: formula("='P&L'!F3", 'currencyBold'),
        2: formula("='P&L'!F7", 'currencyBold'),
        3: formula("='P&L'!F9", 'currencyBold'),
        4: cell('—'),
      },
      4: {
        0: cell('Best case (+12%)'),
        1: formula('=B4*Assumptions!B7', 'currencyBold'),
        2: formula('=B5*(1-Assumptions!B2)-Assumptions!B3*12', 'currencyBold'),
        3: formula('=C5*(1-Assumptions!B4)', 'currencyBold'),
        4: formula('=(B5/B4)-1', 'pctBold'),
      },
      5: {
        0: cell('Worst case (-12%)'),
        1: formula('=B4*Assumptions!B8', 'currencyBold'),
        2: formula('=B6*(1-Assumptions!B2)-Assumptions!B3*12', 'currencyBold'),
        3: formula('=C6*(1-Assumptions!B4)', 'currencyBold'),
        4: formula('=(B6/B4)-1', 'pctBold'),
      },
      7: { 0: cell('Sensitivity: +1% COGS impact on EBITDA'), 1: formula('=-Assumptions!B5*0.01', 'currencyBold') },
      8: { 0: cell('Break-even revenue (approx.)'), 1: formula('=Assumptions!B3*12/(1-Assumptions!B2)', 'currencyBold') },
    },
    mergeData: [{ startRow: 0, endRow: 0, startColumn: 0, endColumn: 4 }],
    columnData: { 0: { w: 220 }, 1: { w: 120 }, 4: { w: 90 } },
    rowCount: 40,
    columnCount: 8,
  }
}

function withSheetDefaults(sheet) {
  return {
    hidden: 0,
    tabColor: sheet.tabColor ?? '',
    freeze: sheet.freeze ?? { xSplit: 0, ySplit: 0, startRow: 0, startColumn: 0 },
    showGridlines: 1,
    rowHeader: { width: 46, hidden: 0 },
    columnHeader: { height: 20, hidden: 0 },
    rightToLeft: 0,
    defaultColumnWidth: 73,
    defaultRowHeight: 23,
    mergeData: [],
    rowData: {},
    ...sheet,
  }
}

export function buildPowerWorkbookData() {
  return {
    id: 'digitorn-fy2026-model',
    name: 'FY2026 Financial Model.xlsx',
    appVersion: '0.25.1',
    locale: 'enUS',
    styles,
    sheetOrder: ['dashboard', 'sales', 'pl', 'assumptions', 'scenarios'],
    sheets: {
      dashboard: withSheetDefaults(buildDashboardSheet()),
      sales: withSheetDefaults(buildSalesSheet()),
      pl: withSheetDefaults(buildPLSheet()),
      assumptions: withSheetDefaults(buildAssumptionsSheet()),
      scenarios: withSheetDefaults(buildScenariosSheet()),
    },
  }
}
