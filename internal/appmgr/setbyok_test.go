package appmgr_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mbathepaul/digitorn/internal/appmgr"
)

func TestSetBYOK_FlipsFlagAndPublishesSnapshot(t *testing.T) {
	m, _, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, src, "chat", nil)

	ctx := context.Background()
	if _, err := m.Install(ctx, src, ""); err != nil {
		t.Fatal(err)
	}

	// Default state : BYOK is false.
	ra, err := m.Get(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if ra.Meta.BYOK {
		t.Fatal("BYOK should default to false after install")
	}

	// Flip to true.
	if err := m.SetBYOK(ctx, "chat", true); err != nil {
		t.Fatalf("SetBYOK true : %v", err)
	}
	ra, err = m.Get(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if !ra.Meta.BYOK {
		t.Error("snapshot should observe BYOK=true after SetBYOK")
	}

	// DB persisted ?
	meta, err := m.GetApp(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.BYOK {
		t.Error("DB row should persist BYOK=true")
	}

	// Flip back to false.
	if err := m.SetBYOK(ctx, "chat", false); err != nil {
		t.Fatalf("SetBYOK false : %v", err)
	}
	ra, _ = m.Get(ctx, "chat")
	if ra.Meta.BYOK {
		t.Error("snapshot should observe BYOK=false after second flip")
	}
}

func TestSetBYOK_Idempotent(t *testing.T) {
	m, _, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, src, "chat", nil)

	ctx := context.Background()
	if _, err := m.Install(ctx, src, ""); err != nil {
		t.Fatal(err)
	}
	// Setting to current value should be a no-op (no error).
	if err := m.SetBYOK(ctx, "chat", false); err != nil {
		t.Fatalf("idempotent SetBYOK(false) : %v", err)
	}
	if err := m.SetBYOK(ctx, "chat", true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBYOK(ctx, "chat", true); err != nil {
		t.Fatalf("idempotent SetBYOK(true) : %v", err)
	}
}

func TestSetBYOK_UnknownAppReturnsNotFound(t *testing.T) {
	m, _, _ := newTestManager(t)
	err := m.SetBYOK(context.Background(), "ghost-app", true)
	if !errors.Is(err, appmgr.ErrAppNotFound) {
		t.Fatalf("err = %v, want ErrAppNotFound", err)
	}
}

func TestSetBYOK_DisabledAppPersistsButSkipsSnapshot(t *testing.T) {
	m, _, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, src, "chat", nil)

	ctx := context.Background()
	if _, err := m.Install(ctx, src, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable(ctx, "chat"); err != nil {
		t.Fatal(err)
	}

	// Disabled app : SetBYOK still updates DB, no snapshot to update.
	if err := m.SetBYOK(ctx, "chat", true); err != nil {
		t.Fatalf("SetBYOK on disabled app : %v", err)
	}
	meta, err := m.GetApp(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.BYOK {
		t.Error("DB row should persist BYOK=true even for disabled app")
	}

	// Re-enable should pick up the new BYOK value.
	if err := m.Enable(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	ra, err := m.Get(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if !ra.Meta.BYOK {
		t.Error("re-enable should restore the BYOK=true set while disabled")
	}
}

func TestSetBYOK_SurvivesReinstall(t *testing.T) {
	// Re-installing the same app should NOT reset BYOK — that flag is
	// an operator decision, independent of bundle contents.
	m, _, _ := newTestManager(t)
	src := filepath.Join(t.TempDir(), "chat")
	writeMinimalApp(t, src, "chat", nil)

	ctx := context.Background()
	if _, err := m.Install(ctx, src, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBYOK(ctx, "chat", true); err != nil {
		t.Fatal(err)
	}
	// Reinstall from the same source (upgrade flow).
	if _, err := m.Install(ctx, src, ""); err != nil {
		t.Fatalf("reinstall : %v", err)
	}
	ra, err := m.Get(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if !ra.Meta.BYOK {
		t.Error("re-install must NOT reset BYOK (operator decision is sticky)")
	}
}
