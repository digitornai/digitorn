// Package http implements the http module.
//
// TODO: port from the Python reference in
//
//	digitorn-bridge/packages/digitorn/modules/http/
//
// Construction pattern (mirrors filesystem and shell):
//
//	m := &Module{}
//	m.Base = module.Base{ID: "http", Version: "0.1.0", Description: "..."}
//	m.RegisterTool(module.Tool{Name: "...", Handler: m.something})
//	return m
package http
