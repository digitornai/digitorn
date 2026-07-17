package server

import (
	"net/http"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

func (d *Daemon) listTemplates(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	out := []map[string]any{}
	if d.appMgr != nil {
		if rt, err := d.appMgr.Get(r.Context(), appID); err == nil && rt != nil && rt.Definition != nil {
			for _, t := range rt.Definition.Templates {
				if t.ID == "" {
					continue
				}
				item := map[string]any{
					"id":          t.ID,
					"name":        t.Name,
					"description": t.Description,
				}
				if t.PreviewPath != "" {
					item["preview_url"] = "/api/apps/" + appID + "/templates/" + t.ID + "/preview/"
				}
				out = append(out, item)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

func (d *Daemon) serveTemplatePreview(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	tplID := chi.URLParam(r, "template_id")
	if d.appMgr == nil {
		writeError(w, http.StatusNotFound, "not_found", "no app manager")
		return
	}
	rt, err := d.appMgr.Get(r.Context(), appID)
	if err != nil || rt == nil || rt.Definition == nil {
		writeError(w, http.StatusNotFound, "not_found", "app not found")
		return
	}
	var previewPath string
	for _, t := range rt.Definition.Templates {
		if t.ID == tplID {
			previewPath = t.PreviewPath
			break
		}
	}
	if previewPath == "" {
		writeError(w, http.StatusNotFound, "not_found", "template preview not found")
		return
	}
	previewPath = filepath.ToSlash(previewPath)
	distDir := path.Dir(previewPath)
	rel := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	if rel == "" {
		rel = path.Base(previewPath)
	}
	base := filepath.Clean(rt.BundleDir)
	abs := filepath.Clean(filepath.Join(base, filepath.FromSlash(distDir), filepath.FromSlash(rel)))
	if abs != base && !strings.HasPrefix(abs, base+string(filepath.Separator)) {
		writeError(w, http.StatusForbidden, "forbidden", "path escapes app bundle")
		return
	}
	d.serveStaticFile(w, r, abs)
}
