package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
	"github.com/mbathepaul/digitorn/internal/usersnippets"
)

func newSnippetsHarness(t *testing.T) *chi.Mux {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&models.UserSnippet{}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{userSnippets: usersnippets.NewStore(gdb)}
	r := chi.NewRouter()
	r.Use(authMiddleware)
	r.Get("/api/apps/{app_id}/snippets", d.listSnippets)
	r.Post("/api/apps/{app_id}/snippets", d.createSnippet)
	r.Patch("/api/apps/{app_id}/snippets/{snippet_id}", d.updateSnippet)
	r.Delete("/api/apps/{app_id}/snippets/{snippet_id}", d.deleteSnippet)
	return r
}

func snipDo(t *testing.T, mux *chi.Mux, method, path, user, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if user != "" {
		req.Header.Set("X-User-ID", user)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	if b := rec.Body.Bytes(); len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return rec.Code, out
}

func TestSnippets_CRUD_Lifecycle(t *testing.T) {
	mux := newSnippetsHarness(t)

	// empty list
	code, body := snipDo(t, mux, "GET", "/api/apps/app-1/snippets", "user-A", "")
	if code != http.StatusOK || body["count"].(float64) != 0 {
		t.Fatalf("empty list: %d %v", code, body)
	}

	// create with emoji + tags
	code, body = snipDo(t, mux, "POST", "/api/apps/app-1/snippets", "user-A",
		`{"title":"Greeting","body":"Hello there","emoji":"👋","tags":["hi","intro"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	sn, _ := body["snippet"].(map[string]any)
	id, _ := sn["id"].(string)
	if id == "" || sn["title"] != "Greeting" {
		t.Fatalf("created snippet: %v", sn)
	}
	if tags, _ := sn["tags"].([]any); len(tags) != 2 {
		t.Fatalf("tags not returned: %v", sn["tags"])
	}

	// update (partial: body only)
	code, body = snipDo(t, mux, "PATCH", "/api/apps/app-1/snippets/"+id, "user-A", `{"body":"Hi!"}`)
	if code != http.StatusOK {
		t.Fatalf("update: %d %v", code, body)
	}
	sn, _ = body["snippet"].(map[string]any)
	if sn["body"] != "Hi!" || sn["title"] != "Greeting" {
		t.Fatalf("partial update wrong: %v", sn)
	}

	// list shows one
	_, body = snipDo(t, mux, "GET", "/api/apps/app-1/snippets", "user-A", "")
	if body["count"].(float64) != 1 {
		t.Fatalf("count=%v want 1", body["count"])
	}

	// delete
	code, body = snipDo(t, mux, "DELETE", "/api/apps/app-1/snippets/"+id, "user-A", "")
	if code != http.StatusOK || body["deleted"] != true {
		t.Fatalf("delete: %d %v", code, body)
	}
}

func TestSnippets_Errors(t *testing.T) {
	mux := newSnippetsHarness(t)
	// missing title/body → 400
	if code, _ := snipDo(t, mux, "POST", "/api/apps/app-1/snippets", "user-A", `{"title":"","body":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("empty title status=%d want 400", code)
	}
	if code, _ := snipDo(t, mux, "POST", "/api/apps/app-1/snippets", "user-A", `{"title":"t","body":""}`); code != http.StatusBadRequest {
		t.Fatalf("empty body status=%d want 400", code)
	}
	// update/delete missing → 404
	if code, _ := snipDo(t, mux, "PATCH", "/api/apps/app-1/snippets/nope", "user-A", `{"title":"z"}`); code != http.StatusNotFound {
		t.Fatalf("update missing status=%d want 404", code)
	}
	if code, _ := snipDo(t, mux, "DELETE", "/api/apps/app-1/snippets/nope", "user-A", ""); code != http.StatusNotFound {
		t.Fatalf("delete missing status=%d want 404", code)
	}
}

func TestSnippets_PerUserIsolation(t *testing.T) {
	mux := newSnippetsHarness(t)
	snipDo(t, mux, "POST", "/api/apps/app-1/snippets", "user-A", `{"title":"mine","body":"x"}`)
	_, body := snipDo(t, mux, "GET", "/api/apps/app-1/snippets", "user-B", "")
	if body["count"].(float64) != 0 {
		t.Fatalf("user-B sees %v snippets", body["count"])
	}
	// And user-B cannot delete user-A's snippet.
	_, a := snipDo(t, mux, "GET", "/api/apps/app-1/snippets", "user-A", "")
	list, _ := a["snippets"].([]any)
	first, _ := list[0].(map[string]any)
	id, _ := first["id"].(string)
	if code, _ := snipDo(t, mux, "DELETE", "/api/apps/app-1/snippets/"+id, "user-B", ""); code != http.StatusNotFound {
		t.Fatalf("cross-user delete status=%d want 404", code)
	}
}
