package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyTreeNoClobber(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	mustWrite(t, filepath.Join(src, "package.json"), "from-template")
	mustWrite(t, filepath.Join(src, "src", "App.tsx"), "template-app")
	// Pre-existing user file with the same path must NOT be overwritten.
	mustWrite(t, filepath.Join(dst, "package.json"), "user-edited")

	if err := copyTreeNoClobber(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "package.json")); got != "user-edited" {
		t.Errorf("clobbered existing file: got %q", got)
	}
	if got := read(t, filepath.Join(dst, "src", "App.tsx")); got != "template-app" {
		t.Errorf("nested seed file not copied: got %q", got)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
