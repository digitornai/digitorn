#!/usr/bin/env bun
// build-piece.ts — Bundle one Activepieces community piece into a standalone .js.
//
// Usage:
//   bun build-piece.ts github [output/github.js]
//   ACTIVEPIECES_DIR=/path/to/ap bun build-piece.ts slack [/tmp/slack.js]
//
// The output .js is a self-contained ESM bundle loadable by the AP bridge.
// Drop it in DIGITORN_PIECES_DIR and restart the bridge.

import { join, dirname, basename } from 'node:path'
import { existsSync, mkdirSync } from 'node:fs'

const pieceName = process.argv[2]
if (!pieceName) {
  process.stderr.write(
    'Usage: bun build-piece.ts <piece-name> [out.js]\n' +
    '  ACTIVEPIECES_DIR defaults to <monorepo-root>\n',
  )
  process.exit(1)
}

const AP_DEFAULT = join(import.meta.dir, '..', '..', '..', 'activepieces-main')
const apRoot = (process.env.ACTIVEPIECES_DIR ?? AP_DEFAULT).replace(/\\/g, '/')

const entry = join(apRoot, 'packages', 'pieces', 'community', pieceName, 'src', 'index.ts')
if (!existsSync(entry)) {
  process.stderr.write(`Piece not found: ${entry}\n`)
  process.exit(1)
}

const outFile = process.argv[3] ?? join(import.meta.dir, 'dist', `${pieceName}.js`)
mkdirSync(dirname(outFile), { recursive: true })

const result = await Bun.build({
  entrypoints: [entry],
  outdir: dirname(outFile),
  naming: { entry: '[name].[ext]' },
  format: 'esm',
  target: 'bun',
  minify: false,
  external: ['node:*', 'bun:*'],
  alias: {
    '@activepieces/pieces-framework': join(apRoot, 'packages', 'pieces', 'framework', 'src', 'index.ts'),
    '@activepieces/pieces-common':    join(apRoot, 'packages', 'pieces', 'common',    'src', 'index.ts'),
    '@activepieces/shared':           join(apRoot, 'packages', 'shared',              'src', 'index.ts'),
  },
})

if (!result.success) {
  for (const msg of result.logs) process.stderr.write(msg.message + '\n')
  process.exit(1)
}

const builtPath = result.outputs[0]?.path
if (builtPath && builtPath !== outFile) {
  // Bun names output after the entry filename; rename to match piece name
  const { renameSync } = await import('node:fs')
  try { renameSync(builtPath, outFile) } catch { /* already correct path */ }
}

process.stdout.write(`built: ${outFile} (${result.outputs[0]?.size ?? '?'} bytes)\n`)

// Emit the sidecar manifest so the lazy loader can list this connector without
// importing the heavy bundle. Best-effort: a build is still useful without it.
try {
  const { writeFileSync } = await import('node:fs')
  const mod = (await import(outFile)) as Record<string, unknown>
  const piece = Object.values(mod).find(
    v => v && typeof v === 'object' && typeof (v as { metadata?: unknown }).metadata === 'function',
  ) as { metadata(): { displayName?: string; description?: string; logoUrl?: string } } | undefined
  if (piece) {
    const meta = piece.metadata()
    const id = basename(outFile, '.js').toLowerCase().replace(/-/g, '_')
    const manifest = {
      id,
      displayName: meta.displayName ?? '',
      description: meta.description ?? '',
      logoUrl: meta.logoUrl ?? '',
      metadata: JSON.parse(JSON.stringify(meta)),
    }
    const metaOut = join(dirname(outFile), `${basename(outFile, '.js')}.meta.json`)
    writeFileSync(metaOut, JSON.stringify(manifest))
    process.stdout.write(`manifest: ${metaOut}\n`)
  }
} catch (e) {
  process.stderr.write(`manifest generation skipped: ${e}\n`)
}
