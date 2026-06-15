package lsp

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

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

// toDiagnosticsBytes is the production conversion: it normalizes diagnostic
// columns to BYTES regardless of the position encoding the server uses.
//
//   - server speaks "utf-8" → columns are already byte offsets; no conversion.
//   - server speaks "utf-16" (LSP default) → columns are UTF-16 code units; we
//     walk the file content to map them to byte offsets so a diagnostic past an
//     emoji or a surrogate pair points at the right byte, not a few units off.
//
// When content is empty (we never received it for this file), we fall back to
// the raw value — better an approximate column than no diagnostic at all.
func toDiagnosticsBytes(file string, raw []lspDiagnostic, content, encoding string) []Diagnostic {
	out := toDiagnostics(file, raw)
	if encoding == "utf-8" || content == "" {
		return out
	}
	lines := strings.Split(content, "\n")
	for i := range out {
		if i >= len(raw) {
			break
		}
		line0 := raw[i].Range.Start.Line
		if line0 < 0 || line0 >= len(lines) {
			continue
		}
		utf16Col := raw[i].Range.Start.Character
		out[i].Column = utf16ColumnToByteColumn(lines[line0], utf16Col) + 1
	}
	return out
}

// utf16ColumnToByteColumn maps a UTF-16 code-unit offset within one line to a
// byte offset within the same line. Required because LSP's default position
// encoding measures Character in UTF-16 code units (a surrogate pair = 2 units
// = 4 bytes), but downstream consumers want bytes.
func utf16ColumnToByteColumn(line string, utf16Col int) int {
	if utf16Col <= 0 {
		return 0
	}
	byteOffset := 0
	utf16Count := 0
	for _, r := range line {
		if utf16Count >= utf16Col {
			break
		}
		size := 1
		if r > 0xFFFF {
			size = 2 // outside the BMP → surrogate pair in UTF-16
		}
		utf16Count += size
		byteOffset += utf8.RuneLen(r)
	}
	return byteOffset
}
