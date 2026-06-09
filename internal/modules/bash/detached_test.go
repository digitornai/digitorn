package bash

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// TestDetachedArgs_EncodedCommand verifies that detachedArgs produces a valid
// -EncodedCommand for PowerShell: the base64 must decode to the original command
// via UTF-16LE (PowerShell's native encoding). Without UTF-16LE, curly braces
// and quotes inside the command produce garbage characters and
// "Unexpected token '}'" errors.
func TestDetachedArgs_EncodedCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"simple", `Write-Output "hello"`},
		{"double-quotes-inside", `cmd /c "exit 0" && Write-Output "ANDOK"`},
		{"hash-literal", `$h = @{a=1; b=2} && Write-Output "HASH-A=$($h.a)"`},
		{"export-and-chain", `export NODE_OPTIONS=--no-warnings; node -e "console.log('test')" 2>/dev/null && Write-Output "OK"`},
		{"fallback-or", `cmd /c "exit 42" || Write-Output "FALLBACK=$LASTEXITCODE"`},
		{"nested-scriptblock", `Get-Service | Where-Object { $_.Status -eq 'Running'; $_.StartType -eq 'Automatic' } | Select-Object -First 2`},
		{"percent-in-string", `$pct = 18.2; Write-Output ("$pct% de libre")`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := detachedArgs("powershell", tt.command)
			if len(args) < 4 || args[0] != "-NoProfile" || args[1] != "-NonInteractive" || args[2] != "-EncodedCommand" {
				t.Fatalf("unexpected args: %v", args)
			}
			// Decode the base64 + UTF-16LE and compare to the original
			b64 := args[3]
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				t.Fatalf("base64 decode failed: %v", err)
			}
			if len(raw)%2 != 0 {
				t.Fatalf("decoded bytes not even (not UTF-16LE): len=%d", len(raw))
			}
			// UTF-16LE decode
			u16 := make([]uint16, len(raw)/2)
			for i := range u16 {
				u16[i] = uint16(raw[i*2]) | uint16(raw[i*2+1])<<8
			}
			decoded := string(utf16.Decode(u16))
			if decoded != tt.command {
				t.Errorf("round-trip mismatch:\n  original: %q\n  decoded:  %q", tt.command, decoded)
			}
		})
	}
}

// TestDetached_PowerShellRun verifies that a real PowerShell command launched
// via runDetached (one-shot, -EncodedCommand) executes correctly with quotes,
// && chaining, and hash literals.
func TestDetached_PowerShellRun(t *testing.T) {
	kind, path, err := detectShell("powershell")
	if err != nil || kind != "powershell" {
		t.Skip("no PowerShell available")
	}
	dir := t.TempDir()
	env := buildEnv(nil)

	tests := []struct {
		name    string
		command string
		wantOut string
		wantErr bool
	}{
		{"double-quotes-and-chain", `cmd /c "exit 0" && Write-Output "ANDOK"`, "ANDOK", false},
		{"fallback-or", `cmd /c "exit 1" || Write-Output "FALLBACK"`, "FALLBACK", false},
		{"hash-literal-chain", `$h = @{a=1; b=2} && Write-Output "A=$($h.a)"`, "A=1", false},
		{"export-and-null", `export X=1; Write-Output "X=$env:X" 2>$null`, "X=1", false},
		{"percent-in-string", `Write-Output ("50% de libre")`, "50% de libre", false},
		{"inline-env", `MYVAR=custom node -e "console.log('INLINE='+process.env.MYVAR)"`, "INLINE=custom", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Apply the same translation pipeline as module.run
			cmd := psNulSink(psChain(psEnv(tt.command)))
			res, err := runDetached(context.Background(), kind, path, cmd, dir, env, 1<<20, "", 15*time.Second)
			if tt.wantErr {
				if err == nil && res.ExitCode == 0 {
					t.Errorf("expected error, got exit=%d stdout=%q", res.ExitCode, res.Stdout)
				}
				return
			}
			if err != nil {
				t.Fatalf("runDetached error: %v", err)
			}
			if res.ExitCode != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
			}
			if tt.wantOut != "" && !strings.Contains(res.Stdout, tt.wantOut) {
				t.Errorf("stdout missing %q: got %q", tt.wantOut, res.Stdout)
			}
		})
	}
}
