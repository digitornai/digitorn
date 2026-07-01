/**
 * Post-load enhancements via Univer Facade API.
 * Each block is isolated so a failure never blocks the workbook from opening.
 */
export function enhanceWorkbook(univerAPI) {
  const workbook = univerAPI.getActiveWorkbook()
  if (!workbook) return

  const tasks = [
    () => enhanceSalesSheet(workbook, univerAPI),
    () => enhanceDashboardSheet(workbook),
    () => enhanceScenariosSheet(workbook),
  ]

  for (const task of tasks) {
    try {
      task()
    } catch (err) {
      console.warn('[univer-demo] enhancement skipped:', err)
    }
  }
}

function enhanceSalesSheet(workbook, univerAPI) {
  const sheet = workbook.getSheetByName('Sales')
  if (!sheet) return

  sheet.getRange('A2:O8').createFilter()

  sheet.getRange('P2').setValue('Region filter demo')
  sheet.getRange('P3:P8').setValues([
    ['North'],
    ['North'],
    ['South'],
    ['East'],
    ['West'],
    ['North'],
  ])

  const regionRule = univerAPI
    .newDataValidation()
    .requireValueInList(['North', 'South', 'East', 'West'], false, true)
    .setOptions({ allowBlank: true, showErrorMessage: true })
    .build()
  sheet.getRange('P3:P8').setDataValidation(regionRule)

  const growthRule = sheet
    .newConditionalFormattingRule()
    .whenNumberGreaterThan(0.25)
    .setRanges([sheet.getRange('O3:O7').getRange()])
    .setBackground('#C6EFCE')
    .setFontColor('#217346')
    .setBold(true)
    .build()
  sheet.addConditionalFormattingRule(growthRule)
}

function enhanceDashboardSheet(workbook) {
  const sheet = workbook.getSheetByName('Dashboard')
  if (!sheet) return

  const belowTarget = sheet
    .newConditionalFormattingRule()
    .whenNumberLessThan(0)
    .setRanges([sheet.getRange('D4:D7').getRange()])
    .setBackground('#FEE4E2')
    .setFontColor('#B42318')
    .setBold(true)
    .build()
  sheet.addConditionalFormattingRule(belowTarget)

  const aboveTarget = sheet
    .newConditionalFormattingRule()
    .whenNumberGreaterThan(0)
    .setRanges([sheet.getRange('D4:D7').getRange()])
    .setBackground('#D1FADF')
    .setFontColor('#027A48')
    .build()
  sheet.addConditionalFormattingRule(aboveTarget)
}

function enhanceScenariosSheet(workbook) {
  const sheet = workbook.getSheetByName('Scenarios')
  if (!sheet) return

  const vsBase = sheet
    .newConditionalFormattingRule()
    .whenNumberGreaterThan(0)
    .setRanges([sheet.getRange('E5:E6').getRange()])
    .setBackground('#E2EFDA')
    .setFontColor('#217346')
    .build()
  sheet.addConditionalFormattingRule(vsBase)

  const vsBaseNeg = sheet
    .newConditionalFormattingRule()
    .whenNumberLessThan(0)
    .setRanges([sheet.getRange('E5:E6').getRange()])
    .setBackground('#FEE4E2')
    .setFontColor('#B42318')
    .build()
  sheet.addConditionalFormattingRule(vsBaseNeg)
}

export function focusDashboard(workbook) {
  if (!workbook) return
  const dashboard = workbook.getSheetByName('Dashboard')
  if (!dashboard) {
    console.warn('[univer-demo] Dashboard sheet missing; tabs:', workbook.getSheets().map((s) => s.getSheetName()))
    return
  }
  workbook.setActiveSheet(dashboard)
  dashboard.getRange('A1').activate()
}
