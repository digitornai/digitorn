package bash

import (
	"strings"
	"testing"
)

// ─── splitStatements ───────────────────────────────────────────────────────────

func TestSplitStatements_Simple(t *testing.T) {
	tests := []struct {
		in      string
		wantN   int
		wantSep string // first separator, if any
	}{
		{`echo hello`, 1, ""},
		{`echo a; echo b`, 2, ";"},
		{`echo a && echo b`, 2, "&&"},
		{`echo a || echo b`, 2, "||"},
		{`echo a; echo b; echo c`, 3, ";"},
		{`echo a && echo b && echo c`, 3, "&&"},
	}
	for _, tt := range tests {
		parts, seps := splitStatements(tt.in)
		if len(parts) != tt.wantN {
			t.Errorf("splitStatements(%q): got %d parts, want %d\n  parts=%v seps=%v", tt.in, len(parts), tt.wantN, parts, seps)
		}
		if tt.wantSep != "" && len(seps) > 0 && seps[0] != tt.wantSep {
			t.Errorf("splitStatements(%q): first sep = %q, want %q", tt.in, seps[0], tt.wantSep)
		}
	}
}

func TestSplitStatements_QuotesArePreserved(t *testing.T) {
	// Semicolons inside quotes must NOT split.
	parts, _ := splitStatements(`echo "a;b"`)
	if len(parts) != 1 {
		t.Errorf("split inside double quotes: got %d parts: %v", len(parts), parts)
	}
	parts2, _ := splitStatements(`echo 'a;b'`)
	if len(parts2) != 1 {
		t.Errorf("split inside single quotes: got %d parts: %v", len(parts2), parts2)
	}
}

func TestSplitStatements_HashLiteral_Semicolon(t *testing.T) {
	// THE BUG: a ; inside @{...} was treated as statement separator.
	// Now that { } track depth, the whole hash stays as one part.
	input := `$events = Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625} -MaxEvents 10`
	parts, seps := splitStatements(input)
	if len(parts) != 1 {
		t.Errorf("splitStatements cut the hash literal: got %d parts\n  parts=%v\n  seps=%v\n  input=%q", len(parts), parts, seps, input)
	}
	if len(seps) != 0 {
		t.Errorf("expected 0 separators inside hash literal, got %v", seps)
	}
	// Verify the reconstructed text is unchanged  (parts + seps rebuild the original)
	got := rebuild(parts, seps)
	if got != input {
		t.Errorf("rebuild mismatch:\n  want %q\n  got  %q", input, got)
	}
}

func TestSplitStatements_ScriptBlock_Semicolon(t *testing.T) {
	// A ; inside { } scriptblock must not split either.
	input := `Get-ChildItem | Where-Object { $_.Length -gt 1kb; $_.Name -like '*.go' } | Select-Object Name`
	parts, seps := splitStatements(input)
	if len(parts) != 1 {
		t.Errorf("splitStatements cut the scriptblock: got %d parts\n  parts=%v\n  seps=%v", len(parts), parts, seps)
	}
}

func TestSplitStatements_NestedBraces(t *testing.T) {
	input := `$h = @{a=@{b="c;d"}; e='f'}`
	parts, seps := splitStatements(input)
	if len(parts) != 1 {
		t.Errorf("splitStatements cut nested hash: got %d parts\n  parts=%v", len(parts), parts)
	}
	_ = seps
}

func TestSplitStatements_MixedBraceParen(t *testing.T) {
	// Braces at depth 1, parentheses at depth 1 — the semicolons are inside both.
	input := `if ($x -and (Test-Path "a;b")) { Write-Output "ok"; $y = 3 }`
	parts, _ := splitStatements(input)
	if len(parts) != 1 {
		t.Errorf("splitStatements cut mixed brace/paren: got %d parts\n  parts=%v", len(parts), parts)
	}
}

func TestSplitStatements_TrueSemicolonsOutside(t *testing.T) {
	// Real statement separators at depth 0 must still split.
	input := `$a = 1; $b = 2; $c = 3`
	parts, seps := splitStatements(input)
	if len(parts) != 3 {
		t.Errorf("expected 3 parts, got %d: %v", len(parts), parts)
	}
	if len(seps) != 2 {
		t.Errorf("expected 2 seps, got %v", seps)
	}
	got := rebuild(parts, seps)
	if got != input {
		t.Errorf("rebuild mismatch:\n  want %q\n  got  %q", input, got)
	}
}

func TestSplitStatements_TrueSemicolonsWithHashBefore(t *testing.T) {
	// A hash literal then a real semicolon outside.
	input := `$h = @{a=1; b=2}; Write-Output "done"`
	parts, seps := splitStatements(input)
	if len(parts) != 2 {
		t.Errorf("expected 2 parts, got %d: %v", len(parts), parts)
	}
	if len(seps) != 1 || seps[0] != ";" {
		t.Errorf("expected sep ';', got %v", seps)
	}
}

// ─── psEnv ──────────────────────────────────────────────────────────────────────

func TestPsEnv_HashLiteralNotTranslated(t *testing.T) {
	// Inline env must NOT match inside a hash literal.
	cases := []struct{ in, want string }{
		// The hash pattern with ID=4625 was being translated to $env:ID='4625}' — BUG
		{`Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625}`,
			`Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625}`},
		// Simple numeric value in hash
		{`Get-WinEvent -FilterHashtable @{LogName='System'; ID=1001}`,
			`Get-WinEvent -FilterHashtable @{LogName='System'; ID=1001}`},
		// Mixed string and numeric
		{`$evts = Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4624,4625}`,
			`$evts = Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4624,4625}`},
	}
	for _, c := range cases {
		if got := psEnv(c.in); got != c.want {
			t.Errorf("psEnv(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

func TestPsEnv_ScriptBlockNotTranslated(t *testing.T) {
	// A Where-Object scriptblock with assignment-like text must not be translated.
	cases := []struct{ in, want string }{
		{`Get-Process | Where-Object { $_.CPU -gt 10 }`,
			`Get-Process | Where-Object { $_.CPU -gt 10 }`},
		{`Get-Service | Where-Object { $_.Status -eq 'Running' }`,
			`Get-Service | Where-Object { $_.Status -eq 'Running' }`},
	}
	for _, c := range cases {
		if got := psEnv(c.in); got != c.want {
			t.Errorf("psEnv(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

func TestPsEnv_RealEnvTranslatedOutsideHash(t *testing.T) {
	// Real env vars before or after a hash should still be translated.
	cases := []struct{ in, want string }{
		{`export FOO=bar; Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625}`,
			`$env:FOO='bar'; Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625}`},
		{`FOO=bar Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625}`,
			`$env:FOO='bar'; Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625}`},
	}
	for _, c := range cases {
		if got := psEnv(c.in); got != c.want {
			t.Errorf("psEnv(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

func TestPsEnv_ExistingCasesRegression(t *testing.T) {
	// All the original TestPsEnv cases must still pass.
	cases := []struct{ in, want string }{
		{`export FOO=bar`, `$env:FOO='bar'`},
		{`export NODE_ENV=production`, `$env:NODE_ENV='production'`},
		{`export FOO="a b"`, `$env:FOO="a b"`},
		{`export FOO='x'`, `$env:FOO='x'`},
		{`export FOO=bar && node x.js`, `$env:FOO='bar' && node x.js`},
		{`FOO=bar node -e "1"`, `$env:FOO='bar'; node -e "1"`},
		{`echo hi; export A=b`, `echo hi; $env:A='b'`},
		{`export PATH=$PATH:/x`, `export PATH=$PATH:/x`},  // $-value → untouched
		{`node --version`, `node --version`},
		{`echo FOO=bar`, `echo FOO=bar`},
	}
	for _, c := range cases {
		if got := psEnv(c.in); got != c.want {
			t.Errorf("psEnv(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

// ─── psChain ─────────────────────────────────────────────────────────────────────

func TestPsChain_WithHashLiteral(t *testing.T) {
	// Chain operators around hash literals must still work.
	cases := []struct{ in, want string }{
		{`$h = @{a=1; b=2} && echo done`,
			`$h = @{a=1; b=2}; if ($?) { echo done }`},
		{`Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625} -MaxEvents 5 || echo "no events"`,
			`Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625} -MaxEvents 5; if (-not $?) { echo "no events" }`},
	}
	for _, c := range cases {
		if got := psChain(c.in); got != c.want {
			t.Errorf("psChain(%q)\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

// ─── Full pipeline: psNulSink(psChain(psEnv(cmd))) ───────────────────────────────

func TestFullPipeline_HashLiteral(t *testing.T) {
	// The exact failing command from the bug report.
	input := `$events = Get-WinEvent -FilterHashtable @{LogName="Security"; ID=4625} -MaxEvents 10`
	got := psNulSink(psChain(psEnv(input)))
	// The hash literal must survive intact — no translation, no splitting.
	if strings.Contains(got, "$env:ID") {
		t.Errorf("hash literal was env-translated: %q", got)
	}
	if strings.Contains(got, "'4625}'") {
		t.Errorf("closing } was swallowed into a quoted value: %q", got)
	}
	if strings.Contains(got, "} -MaxEvents") {
		// The } must still be there and separate from the value
		// Good — check it's proper
	}
}

func TestFullPipeline_ScriptBlockWithSemicolons(t *testing.T) {
	input := `Get-ChildItem | Where-Object { $_.Length -gt 1kb; $_.Name -like '*.go' } | Select-Object Name`
	got := psNulSink(psChain(psEnv(input)))
	// The scriptblock braces must be balanced and not split.
	if strings.Count(got, "{") != strings.Count(got, "}") {
		t.Errorf("unbalanced braces after pipeline: %q", got)
	}
}

func TestFullPipeline_ExportAndHash(t *testing.T) {
	input := `export LOG_LEVEL=debug; Get-WinEvent -FilterHashtable @{LogName='Application'; Level=2} -MaxEvents 3`
	got := psNulSink(psChain(psEnv(input)))
	if !strings.Contains(got, "$env:LOG_LEVEL=") {
		t.Errorf("export not translated: %q", got)
	}
	if strings.Contains(got, "$env:LogName") || strings.Contains(got, "$env:Level") {
		t.Errorf("hash keys were env-translated: %q", got)
	}
}

// ─── Edge: pourcentage dans une chaîne ──────────────────────────────────────────

func TestPsEnv_PercentInString(t *testing.T) {
	// The % operator issue is NOT a translation bug, but psEnv must not corrupt it.
	input := `Write-Output "$([math]::Round(50.0, 1))% de libre"`
	got := psEnv(input)
	// Should stay identical (no env assignment pattern matches)
	if got != input {
		t.Errorf("psEnv corrupted percent-in-string:\n  got  %q\n  want %q", got, input)
	}
	got = psChain(input)
	if got != input {
		t.Errorf("psChain corrupted percent-in-string:\n  got  %q\n  want %q", got, input)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────────

func rebuild(parts, seps []string) string {
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(p)
		if i < len(seps) {
			b.WriteString(seps[i])
		}
	}
	return b.String()
}
