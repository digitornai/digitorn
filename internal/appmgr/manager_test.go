package appmgr_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/appmgr"
	"github.com/mbathepaul/digitorn/internal/compiler"
	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	"github.com/mbathepaul/digitorn/internal/persistence/db"
)

// ----- harness -----

// repoManifestsDir walks up to find the manifests/ dir at the repo
// root (same trick as the compiler tests).
func repoManifestsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			m := filepath.Join(dir, "manifests")
			if st, err := os.Stat(m); err == nil && st.IsDir() {
				return m
			}
		}
		p := filepath.Dir(dir)
		if p == dir {
			break
		}
		dir = p
	}
	t.Fatal("manifests dir not found above CWD")
	return ""
}

// newTestManager spins up a sqlite-in-memory backed manager with a
// fresh apps root under t.TempDir().
func newTestManager(t *testing.T) (appmgr.Manager, string, *gorm.DB) {
	t.Helper()
	// File-based sqlite under t.TempDir() : :memory: + sqlite has
	// per-connection isolation which races under concurrent writes.
	// A real file shared across the gorm connection pool eliminates
	// the flake while staying hermetic to this test.
	dbPath := filepath.Join(t.TempDir(), "appmgr.sqlite")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(gdb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Windows holds the sqlite file open until the gorm connection is
	// closed ; without this, t.TempDir() cleanup fails with EBUSY.
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	root := filepath.Join(t.TempDir(), "apps")
	c := compiler.New().WithSources(catalog.DirSource{Dir: repoManifestsDir(t)})
	m, err := appmgr.New(appmgr.Config{
		DB:       gdb,
		Root:     root,
		Compiler: c,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hub: appmgr.HubConfig{
			URL:             "http://invalid",
			Timeout:         5 * time.Second,
			VerifySSL:       true,
			MaxArchiveBytes: 10 * 1024 * 1024,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, root, gdb
}

// writeMinimalApp creates a source tree with the minimum content the
// compiler accepts. extraFiles writes additional files (testing the
// "copy everything verbatim" behavior).
func writeMinimalApp(t *testing.T, dir, appID string, extraFiles map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "schema_version: 2\n" +
		"app:\n" +
		"  app_id: " + appID + "\n" +
		"  name: " + strings.ToUpper(appID[:1]) + appID[1:] + "\n" +
		"  version: \"0.1.0\"\n" +
		"  description: A test app.\n" +
		"  category: coding\n" +
		"  author: tester@example.com\n" +
		"agents:\n" +
		"  - id: main\n" +
		"    role: worker\n" +
		"    brain:\n" +
		"      provider: anthropic\n" +
		"      model: claude-sonnet-4-6\n" +
		"      config:\n" +
		"        api_key: \"sk-test\"\n" +
		"    system_prompt: hi\n"
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range extraFiles {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ----- TESTS -----

// TestInstall_Local_RoundTrip is the happy path : install from a local
// dir, the app is decoded, written to disk, registered in DB, in cache.
func TestInstall_Local_RoundTrip(t *testing.T) {
	m, root, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, src, "chat", nil)

	ctx := context.Background()
	app, err := m.Install(ctx, src, "")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if app.AppID != "chat" || app.Version != "0.1.0" || !app.Enabled {
		t.Fatalf("unexpected app: %+v", app)
	}
	// .dgc must exist next to source.
	dgcPath := filepath.Join(root, "chat", "app.dgc")
	if _, err := os.Stat(dgcPath); err != nil {
		t.Fatalf("app.dgc not written: %v", err)
	}
	// app.yaml must be copied too.
	if _, err := os.Stat(filepath.Join(root, "chat", "app.yaml")); err != nil {
		t.Fatalf("app.yaml not copied: %v", err)
	}
	// Get hits the cache.
	ra, err := m.Get(ctx, "chat")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ra.Definition.App.AppID != "chat" {
		t.Errorf("Get returned wrong app_id: %s", ra.Definition.App.AppID)
	}
	if !strings.HasSuffix(ra.BundleDir, "chat") {
		t.Errorf("BundleDir not under root: %s", ra.BundleDir)
	}
}

// TestInstall_CopiesEverythingVerbatim : extra files (web/, ui/,
// arbitrary subdirs) must be copied to the install dir.
func TestInstall_CopiesEverythingVerbatim(t *testing.T) {
	m, root, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, src, "chat", map[string]string{
		"web/index.html":         "<html>app</html>",
		"web/assets/icon.svg":    "<svg/>",
		"ui/component.tsx":       "export const X = () => null",
		"random_dir/anyfile.txt": "content",
		"prompts/welcome.md":     "# welcome",
	})

	if _, err := m.Install(context.Background(), src, ""); err != nil {
		t.Fatalf("Install: %v", err)
	}

	want := []string{
		"web/index.html",
		"web/assets/icon.svg",
		"ui/component.tsx",
		"random_dir/anyfile.txt",
		"prompts/welcome.md",
	}
	for _, rel := range want {
		p := filepath.Join(root, "chat", rel)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing copied file: %s : %v", rel, err)
		}
	}
}

// TestInstall_OverwritesPreviousVersion : install of an existing
// app_id replaces files, updates DB, and the old extras are gone.
func TestInstall_OverwritesPreviousVersion(t *testing.T) {
	m, root, _ := newTestManager(t)
	srcA := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, srcA, "chat", map[string]string{
		"web/old.html": "old version",
	})
	if _, err := m.Install(context.Background(), srcA, ""); err != nil {
		t.Fatalf("install A: %v", err)
	}

	srcB := filepath.Join(t.TempDir(), "chat") // different temp parent
	if err := os.MkdirAll(srcB, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalApp(t, srcB, "chat", map[string]string{
		"web/new.html": "new version",
	})
	if _, err := m.Install(context.Background(), srcB, ""); err != nil {
		t.Fatalf("install B: %v", err)
	}

	// New file present, old file gone.
	if _, err := os.Stat(filepath.Join(root, "chat", "web", "new.html")); err != nil {
		t.Errorf("new file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "chat", "web", "old.html")); err == nil {
		t.Error("old file should have been wiped on overwrite")
	}
}

// TestInstall_AppIDMismatchRejected : if the source dir's basename
// doesn't match the YAML app_id, install fails before touching disk.
func TestInstall_AppIDMismatchRejected(t *testing.T) {
	m, _, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "wrong_name")
	writeMinimalApp(t, src, "chat", nil) // app_id=chat, dir=wrong_name

	_, err := m.Install(context.Background(), src, "")
	if !errors.Is(err, appmgr.ErrAppIDMismatch) {
		t.Fatalf("expected ErrAppIDMismatch, got %v", err)
	}
}

// TestInstall_NoYAMLRejected : source dir without app.yaml fails fast.
func TestInstall_NoYAMLRejected(t *testing.T) {
	m, _, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "empty")
	_ = os.MkdirAll(src, 0o755)

	_, err := m.Install(context.Background(), src, "")
	if !errors.Is(err, appmgr.ErrSourceMissingYAML) {
		t.Fatalf("expected ErrSourceMissingYAML, got %v", err)
	}
}

// TestInstall_NonExistentSourceRejected
func TestInstall_NonExistentSourceRejected(t *testing.T) {
	m, _, _ := newTestManager(t)
	_, err := m.Install(context.Background(), "/this/does/not/exist", "")
	if !errors.Is(err, appmgr.ErrBadSource) {
		t.Fatalf("expected ErrBadSource, got %v", err)
	}
}

// TestList_OrderedByName
func TestList_OrderedByName(t *testing.T) {
	m, _, _ := newTestManager(t)
	for _, id := range []string{"zebra", "alpha", "mango"} {
		src := filepath.Join(t.TempDir(), id)
		writeMinimalApp(t, src, id, nil)
		if _, err := m.Install(context.Background(), src, ""); err != nil {
			t.Fatal(err)
		}
	}
	apps, err := m.List(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 3 {
		t.Fatalf("list len: %d", len(apps))
	}
	// "Alpha" < "Mango" < "Zebra" (Title-cased Name).
	if !(apps[0].AppID == "alpha" && apps[1].AppID == "mango" && apps[2].AppID == "zebra") {
		t.Errorf("order: %v %v %v", apps[0].AppID, apps[1].AppID, apps[2].AppID)
	}
}

// TestEnableDisable_TogglesSnapshot : disable removes from cache ; Get
// fails with ErrAppDisabled. Enable repopulates.
func TestEnableDisable_TogglesSnapshot(t *testing.T) {
	m, _, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, src, "chat", nil)
	if _, err := m.Install(context.Background(), src, ""); err != nil {
		t.Fatal(err)
	}

	if err := m.Disable(context.Background(), "chat"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	_, err := m.Get(context.Background(), "chat")
	if !errors.Is(err, appmgr.ErrAppDisabled) {
		t.Fatalf("Get after disable: expected ErrAppDisabled, got %v", err)
	}

	if err := m.Enable(context.Background(), "chat"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	ra, err := m.Get(context.Background(), "chat")
	if err != nil {
		t.Fatalf("Get after enable: %v", err)
	}
	if ra.Meta.AppID != "chat" {
		t.Errorf("Get app_id: %s", ra.Meta.AppID)
	}
}

// TestReload_PicksUpManualEdits : edit app.yaml on disk → reload →
// the new metadata is reflected.
func TestReload_PicksUpManualEdits(t *testing.T) {
	m, root, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, src, "chat", nil)
	if _, err := m.Install(context.Background(), src, ""); err != nil {
		t.Fatal(err)
	}

	// Manually edit the version in the installed yaml.
	yamlPath := filepath.Join(root, "chat", "app.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), `version: "0.1.0"`, `version: "0.2.0"`, 1)
	if err := os.WriteFile(yamlPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Reload(context.Background(), "chat"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	app, err := m.GetApp(context.Background(), "chat")
	if err != nil {
		t.Fatal(err)
	}
	if app.Version != "0.2.0" {
		t.Errorf("version after reload: %s", app.Version)
	}
}

// TestUninstall_RemovesRowAndDir
func TestUninstall_RemovesRowAndDir(t *testing.T) {
	m, root, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, src, "chat", nil)
	if _, err := m.Install(context.Background(), src, ""); err != nil {
		t.Fatal(err)
	}

	if err := m.Uninstall(context.Background(), "chat", false); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "chat")); !os.IsNotExist(err) {
		t.Errorf("install dir should be removed: stat err = %v", err)
	}
	if _, err := m.GetApp(context.Background(), "chat"); !errors.Is(err, appmgr.ErrAppNotFound) {
		t.Errorf("expected ErrAppNotFound, got %v", err)
	}
}

// TestBootstrap_LoadsEnabledOnly : install 2 apps, disable 1, bootstrap
// a fresh manager pointing at the same root+DB ; only the enabled
// shows up in the snapshot.
func TestBootstrap_LoadsEnabledOnly(t *testing.T) {
	m, root, gdb := newTestManager(t)
	for _, id := range []string{"alpha", "beta"} {
		src := filepath.Join(t.TempDir(), id)
		writeMinimalApp(t, src, id, nil)
		if _, err := m.Install(context.Background(), src, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Disable(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}

	// Build a second manager sharing root + DB and bootstrap.
	c := compiler.New().WithSources(catalog.DirSource{Dir: repoManifestsDir(t)})
	m2, err := appmgr.New(appmgr.Config{
		DB: gdb, Root: root, Compiler: c, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hub: appmgr.HubConfig{URL: "http://invalid", Timeout: 5 * time.Second, MaxArchiveBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if _, err := m2.Get(context.Background(), "alpha"); err != nil {
		t.Errorf("alpha should be in snapshot: %v", err)
	}
	if _, err := m2.Get(context.Background(), "beta"); !errors.Is(err, appmgr.ErrAppDisabled) {
		t.Errorf("beta should be disabled: got %v", err)
	}
}

// TestBootstrap_AutoDiscoversDiskAppMissingFromDB : an app installed on disk
// (compiled app.dgc present) whose DB row vanished — the classic "%TEMP% / DB
// got cleared but the bundle survived" case — is auto-discovered at boot,
// loaded, and re-registered enabled. So an installed app is always there after
// a restart without a manual re-install.
func TestBootstrap_AutoDiscoversDiskAppMissingFromDB(t *testing.T) {
	m, root, gdb := newTestManager(t)
	src := filepath.Join(t.TempDir(), "gamma")
	writeMinimalApp(t, src, "gamma", nil)
	if _, err := m.Install(context.Background(), src, ""); err != nil {
		t.Fatal(err)
	}
	// Wipe the DB row, leaving the compiled bundle on disk.
	if err := gdb.Exec("DELETE FROM apps WHERE app_id = ?", "gamma").Error; err != nil {
		t.Fatal(err)
	}

	c := compiler.New().WithSources(catalog.DirSource{Dir: repoManifestsDir(t)})
	m2, err := appmgr.New(appmgr.Config{
		DB: gdb, Root: root, Compiler: c, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hub: appmgr.HubConfig{URL: "http://invalid", Timeout: 5 * time.Second, MaxArchiveBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Auto-loaded into the runtime snapshot...
	if _, err := m2.Get(context.Background(), "gamma"); err != nil {
		t.Errorf("disk app gamma should be auto-loaded at boot: %v", err)
	}
	// ...and re-registered enabled in the DB.
	app, err := m2.GetApp(context.Background(), "gamma")
	if err != nil {
		t.Fatalf("gamma should be re-registered: %v", err)
	}
	if !app.Enabled {
		t.Errorf("auto-discovered app should be enabled, got %+v", app)
	}
}

// TestConcurrent_SameApp_Serialized : 10 concurrent installs of the
// same app — all succeed (last-write-wins), no race, no corrupt dir.
func TestConcurrent_SameApp_Serialized(t *testing.T) {
	m, _, _ := newTestManager(t)
	srcs := make([]string, 10)
	for i := range srcs {
		src := filepath.Join(t.TempDir(), "chat")
		writeMinimalApp(t, src, "chat", nil)
		srcs[i] = src
	}

	var wg sync.WaitGroup
	var failures atomic.Int64
	for _, s := range srcs {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Install(context.Background(), s, ""); err != nil {
				failures.Add(1)
				t.Logf("install: %v", err)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Errorf("%d/%d concurrent installs failed", failures.Load(), len(srcs))
	}
	// Final state must be coherent.
	ra, err := m.Get(context.Background(), "chat")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ra.Definition.App.AppID != "chat" {
		t.Errorf("post-concurrent app_id: %s", ra.Definition.App.AppID)
	}
}

// TestConcurrent_DifferentApps_Parallel : 5 apps installing in
// parallel — each gets its own dir, all visible via List.
func TestConcurrent_DifferentApps_Parallel(t *testing.T) {
	m, _, _ := newTestManager(t)
	var wg sync.WaitGroup
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		i, name := i, name
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := filepath.Join(t.TempDir(), name)
			writeMinimalApp(t, src, name, nil)
			if _, err := m.Install(context.Background(), src, ""); err != nil {
				t.Errorf("install %d %s: %v", i, name, err)
			}
		}()
	}
	wg.Wait()
	apps, _ := m.List(context.Background(), false)
	if len(apps) != 5 {
		t.Errorf("expected 5 apps, got %d", len(apps))
	}
}

// TestHub_InstallFromTarGz : a fake hub serves a tar.gz containing a
// valid app source. The manager extracts, compiles, and installs it.
func TestHub_InstallFromTarGz(t *testing.T) {
	// Build the source tree the hub will serve.
	srcDir := filepath.Join(t.TempDir(), "hub-src")
	writeMinimalApp(t, srcDir, "chat", map[string]string{
		"web/index.html": "<html/>",
	})

	// Build a tar.gz of that dir, with files flat (no top-level wrap).
	archive := buildTarGz(t, srcDir)

	// HTTP test server that returns the archive on /download.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/0.1.0/download"):
			w.Header().Set("Content-Type", "application/gzip")
			w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/packages/digitorn/chat"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"latest_version":"0.1.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Build a manager pointing at the fake hub.
	gdb, _ := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "appmgr.sqlite")), &gorm.Config{})
	_ = db.AutoMigrate(gdb)
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	c := compiler.New().WithSources(catalog.DirSource{Dir: repoManifestsDir(t)})
	m, err := appmgr.New(appmgr.Config{
		DB:       gdb,
		Root:     filepath.Join(t.TempDir(), "apps"),
		Compiler: c,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hub: appmgr.HubConfig{
			URL:             srv.URL,
			Timeout:         5 * time.Second,
			VerifySSL:       false,
			MaxArchiveBytes: 10 * 1024 * 1024,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	app, err := m.Install(context.Background(), "hub://digitorn/chat@0.1.0", "fake-jwt-token")
	if err != nil {
		t.Fatalf("hub install: %v", err)
	}
	if app.AppID != "chat" {
		t.Errorf("hub install app_id: %s", app.AppID)
	}
}

// TestHub_RejectsArchiveTooBig
func TestHub_RejectsArchiveTooBig(t *testing.T) {
	// Serve a "valid" archive but cap the manager's size limit very low.
	srcDir := filepath.Join(t.TempDir(), "src")
	writeMinimalApp(t, srcDir, "chat", map[string]string{
		"big.bin": strings.Repeat("X", 1024*1024), // 1 MB
	})
	archive := buildTarGz(t, srcDir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/"):
			w.Write(archive)
		default:
			w.Write([]byte(`{"latest_version":"0.1.0"}`))
		}
	}))
	defer srv.Close()

	gdb, _ := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "appmgr.sqlite")), &gorm.Config{})
	_ = db.AutoMigrate(gdb)
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	c := compiler.New().WithSources(catalog.DirSource{Dir: repoManifestsDir(t)})
	m, _ := appmgr.New(appmgr.Config{
		DB: gdb, Root: filepath.Join(t.TempDir(), "apps"), Compiler: c,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hub: appmgr.HubConfig{
			URL: srv.URL, Timeout: 5 * time.Second,
			MaxArchiveBytes: 1024, // 1 KB — way smaller than the 1 MB payload
		},
	})

	_, err := m.Install(context.Background(), "hub://digitorn/chat@0.1.0", "")
	if err == nil {
		t.Fatal("expected error for oversized archive")
	}
}

// buildTarGz creates a gzipped tar of dir's contents (members are
// relative paths, no top-level wrap).
func buildTarGz(t *testing.T, dir string) []byte {
	t.Helper()
	var buf strings.Builder
	gz := gzip.NewWriter(&writerAt{&buf})
	tw := tar.NewWriter(gz)

	err := filepath.Walk(dir, func(p string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tar build: %v", err)
	}
	tw.Close()
	gz.Close()
	return []byte(buf.String())
}

// writerAt is a strings.Builder adapter for io.Writer (gzip needs it).
type writerAt struct{ b *strings.Builder }

func (w *writerAt) Write(p []byte) (int, error) { return w.b.Write(p) }
