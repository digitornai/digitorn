//go:build windows

package goshell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWinPath_BackslashCd locks in the tolerance: all THREE cd forms an agent
// emits on Windows now work — an unquoted backslash path (the one POSIX escaping
// used to eat), a quoted backslash path, and a forward-slash path. The agent no
// longer has to remember to quote.
func TestWinPath_BackslashCd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	back := strings.ReplaceAll(sub, "/", "\\") // force backslashes
	fwd := strings.ReplaceAll(sub, "\\", "/")  // force forward slashes

	run := func(script string) (int, string) {
		var out, errb bytes.Buffer
		code, _ := Run(context.Background(), script, root, os.Environ(), nil, &out, &errb)
		return code, strings.TrimSpace(out.String() + errb.String())
	}

	for _, tc := range []struct{ name, script string }{
		{"unquoted-backslash", `cd ` + back + ` && pwd`}, // used to fail; now normalized
		{"quoted-backslash", `cd "` + back + `" && pwd`},
		{"forward-slash", `cd ` + fwd + ` && pwd`},
	} {
		if code, out := run(tc.script); code != 0 {
			t.Fatalf("%s: cd should work, got exit %d: %s", tc.name, code, out)
		}
	}
}

// TestNormalizeWinPaths checks the rewrite directly: drive AND relative path
// separators become forward slashes, while quotes and intentional escapes
// (`\ `, `\$`, `\(`) are preserved.
func TestNormalizeWinPaths(t *testing.T) {
	cases := map[string]string{
		// Drive paths.
		`cd C:\Users\x && node y`: `cd C:/Users/x && node y`,
		`node C:\app\index.js`:    `node C:/app/index.js`,
		`grep foo D:\logs\a.txt`:  `grep foo D:/logs/a.txt`,
		// Relative paths (the gap the drive-only version missed).
		`cat src\index.js`:    `cat src/index.js`,
		`node .\server.js`:    `node ./server.js`,
		`cd a\b\c && pwd`:     `cd a/b/c && pwd`,
		`--config=conf\a.ini`: `--config=conf/a.ini`,
		// Untouched: quoted text.
		`cd "C:\Users\x"`:            `cd "C:\Users\x"`,
		`echo "path is C:\x" && pwd`: `echo "path is C:\x" && pwd`,
		// Untouched: intentional escapes (space / metacharacters).
		`cd My\ Documents`: `cd My\ Documents`,
		`echo \$HOME`:      `echo \$HOME`,
		`grep \( file`:     `grep \( file`,
		// No-ops.
		`echo no backslashes here`: `echo no backslashes here`,
		`cd C:/already/forward`:    `cd C:/already/forward`,
	}
	for in, want := range cases {
		if got := normalizeWinPaths(in); got != want {
			t.Fatalf("normalizeWinPaths(%q) = %q, want %q", in, got, want)
		}
	}
}
