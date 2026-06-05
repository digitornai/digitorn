// Package cron_native implements the cron_native module.
//
// TODO: port from the Python reference in
//
//	digitorn-bridge/packages/digitorn/modules/cron_native/
//
// Construction pattern (mirrors filesystem and shell):
//
//	m := &Module{}
//	m.Base = module.Base{ID: "cron_native", Version: "0.1.0", Description: "..."}
//	m.RegisterTool(module.Tool{Name: "...", Handler: m.something})
//	return m
package cron_native
