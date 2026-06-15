#!/usr/bin/env bun
// digitorn-ap-bridge: exposes Activepieces piece actions as MCP stdio tools.
//
// Environment variables:
//   DIGITORN_PIECES_DIR      Directory containing piece bundles (*.js). Default: ~/.digitorn/pieces
//   DIGITORN_AP_TRIGGER_PORT HTTP port for the trigger server. Default: 9234. Set to 0 to disable.

import { Server } from '@modelcontextprotocol/sdk/server/index.js'
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js'
import { ListToolsRequestSchema, CallToolRequestSchema } from '@modelcontextprotocol/sdk/types.js'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { PieceLoader } from './loader.ts'
import { executeTool } from './executor.ts'
import { TriggerServer } from './trigger-server.ts'

const piecesDir = process.env.DIGITORN_PIECES_DIR ?? join(homedir(), '.digitorn', 'pieces')
const triggerPort = parseInt(process.env.DIGITORN_AP_TRIGGER_PORT ?? '9234', 10)

const loader = new PieceLoader(piecesDir)
await loader.load()

const toolCount = loader.getTools().length
process.stderr.write(`pieces-bridge: loaded ${toolCount} tools from ${piecesDir}\n`)

const server = new Server(
  { name: 'digitorn-ap-bridge', version: '1.0.0' },
  { capabilities: { tools: {} } },
)

server.setRequestHandler(ListToolsRequestSchema, async () => {
  return { tools: loader.getTools() }
})

server.setRequestHandler(CallToolRequestSchema, async (req) => {
  const name = req.params.name
  const args = (req.params.arguments ?? {}) as Record<string, unknown>

  const result = await executeTool(name, args, loader)

  const text = result.success
    ? JSON.stringify({ ok: true, data: result.data })
    : JSON.stringify({ ok: false, error: result.error })

  return {
    content: [{ type: 'text' as const, text }],
    isError: !result.success,
  }
})

// MCP stdio transport first — the bridge must respond to initialize even if
// the trigger HTTP server fails to bind (port conflict, etc.).
const transport = new StdioServerTransport()
await server.connect(transport)

// Trigger server is non-fatal: if port is already in use (e.g. a previous
// bridge instance is still holding it), log a warning and continue. The MCP
// tools layer works fine without it.
if (triggerPort > 0) {
  try {
    const triggerServer = new TriggerServer(loader)
    triggerServer.start(triggerPort)
  } catch (e) {
    process.stderr.write(`pieces-bridge: trigger server on :${triggerPort} failed (${e}) — background triggers disabled\n`)
  }
}
