// Package preview implements the preview module.
//
// TODO: port from the Python reference in
//
//	digitorn-bridge/packages/digitorn/modules/preview/
//
// Construction pattern (mirrors filesystem and shell):
//
//	m := &Module{}
//	m.Base = module.Base{ID: "preview", Version: "0.1.0", Description: "..."}
//	m.RegisterTool(module.Tool{Name: "...", Handler: m.something})
//	return m
package preview
