import { UniverSheetsConditionalFormattingPreset } from '@univerjs/preset-sheets-conditional-formatting'
import { UniverSheetsCorePreset } from '@univerjs/preset-sheets-core'
import { UniverSheetsDataValidationPreset } from '@univerjs/preset-sheets-data-validation'
import { UniverSheetsDrawingPreset } from '@univerjs/preset-sheets-drawing'
import { UniverSheetsFilterPreset } from '@univerjs/preset-sheets-filter'
import { UniverSheetsFindReplacePreset } from '@univerjs/preset-sheets-find-replace'
import { UniverSheetsHyperLinkPreset } from '@univerjs/preset-sheets-hyper-link'
import { UniverSheetsNotePreset } from '@univerjs/preset-sheets-note'
import { UniverSheetsSortPreset } from '@univerjs/preset-sheets-sort'
import { UniverSheetsTablePreset } from '@univerjs/preset-sheets-table'
import UniverPresetSheetsConditionalFormattingEnUS from '@univerjs/preset-sheets-conditional-formatting/locales/en-US'
import UniverPresetSheetsCoreEnUS from '@univerjs/preset-sheets-core/locales/en-US'
import UniverPresetSheetsDataValidationEnUS from '@univerjs/preset-sheets-data-validation/locales/en-US'
import UniverPresetSheetsDrawingEnUS from '@univerjs/preset-sheets-drawing/locales/en-US'
import UniverPresetSheetsFilterEnUS from '@univerjs/preset-sheets-filter/locales/en-US'
import UniverPresetSheetsFindReplaceEnUS from '@univerjs/preset-sheets-find-replace/locales/en-US'
import UniverPresetSheetsHyperLinkEnUS from '@univerjs/preset-sheets-hyper-link/locales/en-US'
import UniverPresetSheetsNoteEnUS from '@univerjs/preset-sheets-note/locales/en-US'
import UniverPresetSheetsSortEnUS from '@univerjs/preset-sheets-sort/locales/en-US'
import UniverPresetSheetsTableEnUS from '@univerjs/preset-sheets-table/locales/en-US'
import { createUniver, greenTheme, LocaleType, mergeLocales } from '@univerjs/presets'

import { enhanceWorkbook, focusDashboard } from './workbook-enhance.js'
import { buildPowerWorkbookData } from './workbook-data.js'

import '@univerjs/preset-sheets-core/lib/index.css'

export function createDemoUniver(container) {
  const { univer, univerAPI } = createUniver({
    theme: greenTheme,
    locale: LocaleType.EN_US,
    locales: {
      [LocaleType.EN_US]: mergeLocales(
        UniverPresetSheetsCoreEnUS,
        UniverPresetSheetsFilterEnUS,
        UniverPresetSheetsSortEnUS,
        UniverPresetSheetsConditionalFormattingEnUS,
        UniverPresetSheetsDataValidationEnUS,
        UniverPresetSheetsTableEnUS,
        UniverPresetSheetsFindReplaceEnUS,
        UniverPresetSheetsHyperLinkEnUS,
        UniverPresetSheetsDrawingEnUS,
        UniverPresetSheetsNoteEnUS,
      ),
    },
    presets: [
      UniverSheetsCorePreset({
        container,
        ribbonType: 'classic',
        header: true,
        toolbar: true,
        formulaBar: true,
        footer: {
          sheetBar: true,
          statisticBar: true,
          zoomSlider: true,
          menus: true,
        },
      }),
      UniverSheetsFilterPreset(),
      UniverSheetsSortPreset(),
      UniverSheetsConditionalFormattingPreset(),
      UniverSheetsDataValidationPreset(),
      UniverSheetsTablePreset(),
      UniverSheetsFindReplacePreset(),
      UniverSheetsHyperLinkPreset(),
      UniverSheetsDrawingPreset(),
      UniverSheetsNotePreset(),
    ],
  })

  const workbook = univerAPI.createWorkbook(buildPowerWorkbookData())
  focusDashboard(workbook)

  // Apply filters / CF after the Dashboard is visible.
  queueMicrotask(() => enhanceWorkbook(univerAPI))

  return { univer, univerAPI, workbook }
}
