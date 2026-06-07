package server

import (
	"os"
	"path/filepath"
	"testing"
)

func writePreviewFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A Vite project keeps a SOURCE index.html at the root (points at /src) next to
// the real built dist/index.html (points at /assets). The built entry must win —
// serving the source template gives a guaranteed blank page.
func TestResolvePreviewEntry_PrefersBuildOverSourceTemplate(t *testing.T) {
	wd := t.TempDir()
	writePreviewFixture(t, filepath.Join(wd, "index.html"), `<script type="module" src="/src/main.jsx"></script>`)
	writePreviewFixture(t, filepath.Join(wd, "dist", "index.html"), `<script type="module" crossorigin src="/assets/index-abc.js"></script>`)
	if got := resolvePreviewEntry(wd); got != "dist/index.html" {
		t.Fatalf("expected dist/index.html, got %q", got)
	}
}

// An unbuilt Vite project (only the source template, no dist) is NOT servable —
// the caller reports "nothing built yet" instead of attaching a blank iframe.
func TestResolvePreviewEntry_UnbuiltVite_NotServable(t *testing.T) {
	wd := t.TempDir()
	writePreviewFixture(t, filepath.Join(wd, "index.html"), `<script type="module" src="/src/main.tsx"></script>`)
	if got := resolvePreviewEntry(wd); got != "" {
		t.Fatalf("an unbuilt Vite template must not be servable, got %q", got)
	}
}

// A hand-written plain static site (no build step, no /src) IS served from root.
func TestResolvePreviewEntry_PlainStaticSite_ServedFromRoot(t *testing.T) {
	wd := t.TempDir()
	writePreviewFixture(t, filepath.Join(wd, "index.html"), "<h1>hello</h1><script src=\"/app.js\"></script>")
	if got := resolvePreviewEntry(wd); got != "index.html" {
		t.Fatalf("a plain static root index.html must be served, got %q", got)
	}
}

// CRA keeps the %PUBLIC_URL% template in public/; the build lands in build/.
func TestResolvePreviewEntry_CRA_PrefersBuild(t *testing.T) {
	wd := t.TempDir()
	writePreviewFixture(t, filepath.Join(wd, "public", "index.html"), `<div>%PUBLIC_URL%/favicon.ico</div>`)
	writePreviewFixture(t, filepath.Join(wd, "build", "index.html"), `<script src="/static/js/main.abc.js"></script>`)
	if got := resolvePreviewEntry(wd); got != "build/index.html" {
		t.Fatalf("expected build/index.html, got %q", got)
	}
}

// Agents scaffold the project in a subdir (my-react-app/); the build there must
// be found even though the workdir root has only the subdir.
func TestResolvePreviewEntry_SubdirBuild(t *testing.T) {
	wd := t.TempDir()
	writePreviewFixture(t, filepath.Join(wd, "my-react-app", "index.html"), `<script src="/src/main.jsx"></script>`)
	writePreviewFixture(t, filepath.Join(wd, "my-react-app", "dist", "index.html"), `<script src="/assets/index-x.js"></script>`)
	// noise that must be ignored
	writePreviewFixture(t, filepath.Join(wd, "node_modules", "pkg", "dist", "index.html"), `<script src="/assets/x.js"></script>`)
	if got := resolvePreviewEntry(wd); got != "my-react-app/dist/index.html" {
		t.Fatalf("expected my-react-app/dist/index.html, got %q", got)
	}
}

// A scaffolded-but-not-built subdir (only the Vite source template) is not
// servable → "" so the caller reports "nothing built yet".
func TestResolvePreviewEntry_SubdirUnbuilt_NotServable(t *testing.T) {
	wd := t.TempDir()
	writePreviewFixture(t, filepath.Join(wd, "my-react-app", "index.html"), `<script src="/src/main.jsx"></script>`)
	writePreviewFixture(t, filepath.Join(wd, "my-react-app", "package.json"), `{}`)
	if got := resolvePreviewEntry(wd); got != "" {
		t.Fatalf("an unbuilt subdir must not be servable, got %q", got)
	}
}

// A build at the workdir root wins over one in a subdir.
func TestResolvePreviewEntry_RootWinsOverSubdir(t *testing.T) {
	wd := t.TempDir()
	writePreviewFixture(t, filepath.Join(wd, "dist", "index.html"), `<script src="/assets/root.js"></script>`)
	writePreviewFixture(t, filepath.Join(wd, "frontend", "dist", "index.html"), `<script src="/assets/sub.js"></script>`)
	if got := resolvePreviewEntry(wd); got != "dist/index.html" {
		t.Fatalf("root build must win, got %q", got)
	}
}
