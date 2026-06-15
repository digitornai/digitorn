package mcpservers

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
	"github.com/mbathepaul/digitorn/internal/server/mcpoauth"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := gdb.AutoMigrate(&models.ManagedMCPServer{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sealer, err := mcpoauth.NewSealer(filepath.Join(t.TempDir(), "test.key"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return NewStore(gdb, sealer)
}

func TestInstallListGetRedaction(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	srv, err := s.Install(ctx, "user-A", Spec{
		ServerID: "GitHub", DisplayName: "GitHub", Source: "catalog", Transport: "stdio",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-github"},
		Secrets: map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_secret_value"},
		AuthType: "token", Package: "@modelcontextprotocol/server-github",
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if srv.ServerID != "github" { // normalized to lowercase
		t.Fatalf("server_id=%q want github", srv.ServerID)
	}
	if srv.ID == "" || srv.CreatedAt.IsZero() {
		t.Fatalf("missing id/timestamps: %+v", srv)
	}
	// The API view must redact secret VALUES to key names only.
	if len(srv.SecretKeys) != 1 || srv.SecretKeys[0] != "GITHUB_PERSONAL_ACCESS_TOKEN" {
		t.Fatalf("secret_keys=%v", srv.SecretKeys)
	}

	list, err := s.List(ctx, "user-A")
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%d err=%v", len(list), err)
	}

	got, found, err := s.Get(ctx, "user-A", "github")
	if err != nil || !found {
		t.Fatalf("get found=%v err=%v", found, err)
	}
	if got.Command != "npx" {
		t.Fatalf("command=%q", got.Command)
	}
}

func TestRevealUnsealsSecrets(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Install(ctx, "user-A", Spec{
		ServerID: "mail", Transport: "stdio", Command: "npx", Args: []string{"-y", "mcp-mail-server"},
		Env:     map[string]string{"SMTP_HOST": "smtp.example.com"},
		Secrets: map[string]string{"EMAIL_PASS": "p@ss-w0rd"},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	view, secrets, found, err := s.Reveal(ctx, "user-A", "mail")
	if err != nil || !found {
		t.Fatalf("reveal found=%v err=%v", found, err)
	}
	if secrets["EMAIL_PASS"] != "p@ss-w0rd" {
		t.Fatalf("revealed secret mismatch: %v", secrets)
	}
	if view.Env["SMTP_HOST"] != "smtp.example.com" {
		t.Fatalf("non-secret env lost: %v", view.Env)
	}
	// The redacted view never carries the value.
	if len(view.SecretKeys) != 1 || view.SecretKeys[0] != "EMAIL_PASS" {
		t.Fatalf("secret_keys=%v", view.SecretKeys)
	}
}

func TestUpdateRotatesSecret(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	first, _ := s.Install(ctx, "user-A", Spec{
		ServerID: "github", Transport: "stdio", Command: "npx",
		Secrets: map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "old"},
	})
	newName := "GitHub prod"
	newSecrets := map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "rotated"}
	upd, err := s.Update(ctx, "user-A", "github", Patch{DisplayName: &newName, Secrets: &newSecrets})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.DisplayName != "GitHub prod" {
		t.Fatalf("display_name=%q", upd.DisplayName)
	}
	if upd.UpdatedAt.Before(first.UpdatedAt) {
		t.Fatalf("updated_at went backwards")
	}
	_, secrets, _, _ := s.Reveal(ctx, "user-A", "github")
	if secrets["GITHUB_PERSONAL_ACCESS_TOKEN"] != "rotated" {
		t.Fatalf("secret not rotated: %v", secrets)
	}
}

func TestConflictAndIsolationAndValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := Spec{ServerID: "github", Transport: "stdio", Command: "npx"}
	if _, err := s.Install(ctx, "user-A", base); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Same user, same id → conflict.
	if _, err := s.Install(ctx, "user-A", base); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	// Different user, same id → allowed (per-user isolation).
	if _, err := s.Install(ctx, "user-B", base); err != nil {
		t.Fatalf("isolation install: %v", err)
	}
	// user-A can't see user-B's server count beyond their own.
	if list, _ := s.List(ctx, "user-A"); len(list) != 1 {
		t.Fatalf("user-A list=%d want 1", len(list))
	}
	// Invalid id.
	if _, err := s.Install(ctx, "user-A", Spec{ServerID: "Bad Id!", Command: "x"}); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("want ErrInvalidID, got %v", err)
	}
	// stdio without command → invalid spec.
	if _, err := s.Install(ctx, "user-A", Spec{ServerID: "nocmd", Transport: "stdio"}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("want ErrInvalidSpec, got %v", err)
	}
	// Delete + not found.
	if err := s.Delete(ctx, "user-A", "github"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, "user-A", "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
