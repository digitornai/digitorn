package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mbathepaul/digitorn/internal/runtime"
	"github.com/mbathepaul/digitorn/internal/runtime/context/meta"
	"github.com/mbathepaul/digitorn/internal/userskills"
)

// skillContentAdapter bridges the meta-package SkillLoader (returns
// meta.SkillEntry) to the runtime engine's SkillLoader (returns
// runtime.SkillContent). runtime can't import meta, so the conversion lives in
// the wiring layer. This is what powers the user-driven path : a message that
// carries a `/command` skill → engine.injectSkillDirective resolves it through
// the SAME layered loader (user skills first, then app bundle) as the agent's
// use_skill tool.
type skillContentAdapter struct{ inner meta.SkillLoader }

func (a skillContentAdapter) Load(ctx context.Context, appID, userID, command string) (runtime.SkillContent, error) {
	e, err := a.inner.Load(ctx, appID, userID, command)
	if err != nil {
		return runtime.SkillContent{}, err
	}
	return runtime.SkillContent{Command: e.Command, Description: e.Description, Content: e.Content}, nil
}

// appDevSkills returns the app's allow_user_skills flag plus its bundled skill
// commands (command + description only — never the content). (false, nil) when
// the app or its dev block is absent.
func (d *Daemon) appDevSkills(ctx context.Context, appID string) (bool, []map[string]string) {
	if d.appMgr == nil {
		return false, nil
	}
	rt, err := d.appMgr.Get(ctx, appID)
	if err != nil || rt == nil || rt.Definition == nil || rt.Definition.Dev == nil {
		return false, nil
	}
	dev := rt.Definition.Dev
	out := make([]map[string]string, 0, len(dev.Skills))
	for _, sk := range dev.Skills {
		if sk.Command == "" {
			continue
		}
		out = append(out, map[string]string{
			"command":     sk.Command,
			"description": sk.Description,
		})
	}
	return dev.AllowUserSkills, out
}

// listSkills returns the union the agent sees : the app's bundled skills (from
// the YAML) + the caller's own authored skills, plus whether authoring is
// allowed for this app.
func (d *Daemon) listSkills(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	allow, appSkills := d.appDevSkills(r.Context(), appID)
	items, err := d.userSkills.List(r.Context(), userID, appID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app_skills":        appSkills,
		"user_skills":       items,
		"allow_user_skills": allow,
	})
}

// createSkill authors a new user skill for this app. Gated on the app's
// dev.allow_user_skills.
func (d *Daemon) createSkill(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	userID := userIDOf(r.Context())
	if allow, _ := d.appDevSkills(r.Context(), appID); !allow {
		writeError(w, http.StatusForbidden, "user_skills_disabled",
			"this app does not allow user-authored skills (set dev.allow_user_skills: true)")
		return
	}
	var req struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Instructions string `json:"instructions"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sk, err := d.userSkills.Create(r.Context(), userID, appID, req.Name, req.Description, req.Instructions)
	if err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"skill": sk})
}

// updateSkill applies a partial change to one of the caller's skills.
func (d *Daemon) updateSkill(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	skillID := chi.URLParam(r, "skill_id")
	userID := userIDOf(r.Context())
	if allow, _ := d.appDevSkills(r.Context(), appID); !allow {
		writeError(w, http.StatusForbidden, "user_skills_disabled",
			"this app does not allow user-authored skills (set dev.allow_user_skills: true)")
		return
	}
	var req struct {
		Name         *string `json:"name"`
		Description  *string `json:"description"`
		Instructions *string `json:"instructions"`
	}
	if err := readJSONLenient(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sk, err := d.userSkills.Update(r.Context(), userID, appID, skillID, req.Name, req.Description, req.Instructions)
	if err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": sk})
}

// deleteSkill hard-deletes one of the caller's skills. Always allowed on an
// owned row (so a user can clean up even after the app disables authoring) ;
// the store returns ErrNotFound for anyone else's row.
func (d *Daemon) deleteSkill(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	skillID := chi.URLParam(r, "skill_id")
	userID := userIDOf(r.Context())
	if err := d.userSkills.Delete(r.Context(), userID, appID, skillID); err != nil {
		writeSkillError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// writeSkillError maps the store's sentinel errors to HTTP status codes.
func writeSkillError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, userskills.ErrNotFound):
		writeError(w, http.StatusNotFound, "skill_not_found", err.Error())
	case errors.Is(err, userskills.ErrNameConflict):
		writeError(w, http.StatusConflict, "skill_name_conflict", err.Error())
	case errors.Is(err, userskills.ErrInvalidName):
		writeError(w, http.StatusUnprocessableEntity, "invalid_skill_name", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "skill_failed", err.Error())
	}
}
