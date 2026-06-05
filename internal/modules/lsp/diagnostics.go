package lsp

import "encoding/json"

// LSP wire types (the diagnostics subset). Positions are 0-based per the spec.

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspDiagnostic struct {
	Range    lspRange        `json:"range"`
	Severity int             `json:"severity"`
	Code     json.RawMessage `json:"code"`
	Source   string          `json:"source"`
	Message  string          `json:"message"`
}

type publishDiagnosticsParams struct {
	URI         string          `json:"uri"`
	Version     int             `json:"version"`
	Diagnostics []lspDiagnostic `json:"diagnostics"`
}

// Diagnostic is the agent-facing shape: 1-based line/column and a textual
// severity, so it reads like a compiler message.
type Diagnostic struct {
	File     string `json:"file"`
	Line     int    `json:"line"`     // 1-based
	Column   int    `json:"column"`   // 1-based
	Severity string `json:"severity"` // error | warning | info | hint
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

func severityName(s int) string {
	switch s {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "error" // unknown/unset → treat as error (fail-loud, never silently downgrade)
	}
}

// toDiagnostics converts raw LSP diagnostics for one file into the agent shape.
func toDiagnostics(file string, raw []lspDiagnostic) []Diagnostic {
	out := make([]Diagnostic, 0, len(raw))
	for _, d := range raw {
		code := ""
		if len(d.Code) > 0 && string(d.Code) != "null" {
			code = string(d.Code)
			if u, err := jsonString(d.Code); err == nil {
				code = u // unquote string codes; numeric codes stay as-is
			}
		}
		out = append(out, Diagnostic{
			File:     file,
			Line:     d.Range.Start.Line + 1,
			Column:   d.Range.Start.Character + 1,
			Severity: severityName(d.Severity),
			Code:     code,
			Source:   d.Source,
			Message:  d.Message,
		})
	}
	return out
}

func jsonString(raw json.RawMessage) (string, error) {
	var s string
	err := json.Unmarshal(raw, &s)
	return s, err
}
