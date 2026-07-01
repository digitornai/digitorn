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

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/persistence/models"
	"github.com/digitornai/digitorn/internal/userskills"
)

func newSkillsHarness(t *testing.T, allow bool) *chi.Mux {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&models.UserSkill{}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		userSkills: userskills.NewStore(gdb),
		appMgr: &fakeAppMgr{apps: map[string]*appmgr.RuntimeApp{
			"app-1": {Definition: &schema.AppDefinition{Dev: &schema.DevBlock{
				AllowUserSkills: allow,
				Skills: []schema.SkillEntry{
					{Command: "/commit", Description: "Make a commit", Path: "skills/commit.md"},
				},
			}}},
		}},
	}
	r := chi.NewRouter()
	r.Use(authMiddleware)
	r.Get("/api/apps/{app_id}/skills", d.listSkills)
	r.Post("/api/apps/{app_id}/skills", d.createSkill)
	r.Patch("/api/apps/{app_id}/skills/{skill_id}", d.updateSkill)
	r.Delete("/api/apps/{app_id}/skills/{skill_id}", d.deleteSkill)
	return r
}

func skillDo(t *testing.T, mux *chi.Mux, method, path, user, body string) (int, map[string]any) {
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

func TestSkills_CRUD_Lifecycle(t *testing.T) {
	mux := newSkillsHarness(t, true)

	// list : empty user skills, but app skills + allow flag present.
	code, body := skillDo(t, mux, "GET", "/api/apps/app-1/skills", "user-A", "")
	if code != http.StatusOK {
		t.Fatalf("list: %d %v", code, body)
	}
	if body["allow_user_skills"] != true {
		t.Fatalf("allow flag: %v", body["allow_user_skills"])
	}
	if app, _ := body["app_skills"].([]any); len(app) != 1 {
		t.Fatalf("app_skills=%v", body["app_skills"])
	}

	// create
	code, body = skillDo(t, mux, "POST", "/api/apps/app-1/skills", "user-A",
		`{"name":"Deploy","description":"ship it","instructions":"run the deploy"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	sk, _ := body["skill"].(map[string]any)
	id, _ := sk["id"].(string)
	if id == "" || sk["name"] != "deploy" {
		t.Fatalf("created skill: %v", sk)
	}

	// update
	code, body = skillDo(t, mux, "PATCH", "/api/apps/app-1/skills/"+id, "user-A",
		`{"instructions":"run the new deploy"}`)
	if code != http.StatusOK {
		t.Fatalf("update: %d %v", code, body)
	}

	// list shows the one user skill now
	_, body = skillDo(t, mux, "GET", "/api/apps/app-1/skills", "user-A", "")
	if us, _ := body["user_skills"].([]any); len(us) != 1 {
		t.Fatalf("user_skills=%v", body["user_skills"])
	}

	// delete
	code, body = skillDo(t, mux, "DELETE", "/api/apps/app-1/skills/"+id, "user-A", "")
	if code != http.StatusOK || body["deleted"] != true {
		t.Fatalf("delete: %d %v", code, body)
	}
}

func TestSkills_Errors(t *testing.T) {
	mux := newSkillsHarness(t, true)

	// invalid slug → 422
	if code, _ := skillDo(t, mux, "POST", "/api/apps/app-1/skills", "user-A",
		`{"name":"Bad Name!","instructions":"x"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid name status=%d want 422", code)
	}
	// duplicate → 409
	skillDo(t, mux, "POST", "/api/apps/app-1/skills", "user-A", `{"name":"dup","instructions":"x"}`)
	if code, _ := skillDo(t, mux, "POST", "/api/apps/app-1/skills", "user-A",
		`{"name":"dup","instructions":"y"}`); code != http.StatusConflict {
		t.Fatalf("dup status=%d want 409", code)
	}
	// update/delete missing → 404
	if code, _ := skillDo(t, mux, "PATCH", "/api/apps/app-1/skills/nope", "user-A", `{"description":"z"}`); code != http.StatusNotFound {
		t.Fatalf("update missing status=%d want 404", code)
	}
	if code, _ := skillDo(t, mux, "DELETE", "/api/apps/app-1/skills/nope", "user-A", ""); code != http.StatusNotFound {
		t.Fatalf("delete missing status=%d want 404", code)
	}
}

func TestSkills_AuthoringDisabled(t *testing.T) {
	mux := newSkillsHarness(t, false) // allow_user_skills = false

	// create blocked with 403
	if code, _ := skillDo(t, mux, "POST", "/api/apps/app-1/skills", "user-A",
		`{"name":"x","instructions":"y"}`); code != http.StatusForbidden {
		t.Fatalf("create disabled status=%d want 403", code)
	}
	// list still works (reports allow=false)
	code, body := skillDo(t, mux, "GET", "/api/apps/app-1/skills", "user-A", "")
	if code != http.StatusOK || body["allow_user_skills"] != false {
		t.Fatalf("list disabled: %d %v", code, body)
	}
}

func TestSkills_PerUserIsolation(t *testing.T) {
	mux := newSkillsHarness(t, true)
	skillDo(t, mux, "POST", "/api/apps/app-1/skills", "user-A", `{"name":"mine","instructions":"a"}`)

	// user-B sees none of user-A's skills.
	_, body := skillDo(t, mux, "GET", "/api/apps/app-1/skills", "user-B", "")
	if us, _ := body["user_skills"].([]any); len(us) != 0 {
		t.Fatalf("user-B sees %v", body["user_skills"])
	}
}
