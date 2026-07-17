package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/digitornai/digitorn/internal/modulesettings"
)

const secretSentinel = "••• set"

type moduleSettingsEntry struct {
	ModuleID    string         `json:"module_id"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"schema"`
	Value       any            `json:"value"`
}

func (d *Daemon) appModuleSettings(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	ra, err := d.appMgr.Get(r.Context(), appID)
	if err != nil || ra == nil || ra.Definition == nil || ra.Definition.Tools == nil {
		writeJSON(w, http.StatusOK, map[string]any{"modules": []any{}, "count": 0})
		return
	}

	userID := userIDOf(r.Context())
	out := make([]moduleSettingsEntry, 0, len(ra.Definition.Tools.Modules))
	for moduleID, block := range ra.Definition.Tools.Modules {
		man, ok := d.modules.Manifest(moduleID)
		if !ok || len(man.ConfigSchema) == 0 {
			continue
		}
		var deltas map[string]any
		if d.moduleSettings != nil {
			deltas = d.moduleSettings.Deltas(r.Context(), userID, appID, moduleID)
		}
		effective := modulesettings.DeepMerge(block.Config, deltas)
		out = append(out, moduleSettingsEntry{
			ModuleID:    moduleID,
			Description: man.Description,
			Schema:      man.ConfigSchema,
			Value:       redactSecrets(effective, man.ConfigSchema),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModuleID < out[j].ModuleID })
	writeJSON(w, http.StatusOK, map[string]any{"modules": out, "count": len(out)})
}

func (d *Daemon) setAppModuleConfig(w http.ResponseWriter, r *http.Request) {
	if d.moduleSettings == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "module settings unavailable (server key missing)")
		return
	}
	appID := chi.URLParam(r, "app_id")
	moduleID := chi.URLParam(r, "module_id")
	userID := userIDOf(r.Context())

	var submitted map[string]any
	if err := readJSONLenient(r, &submitted); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ra, err := d.appMgr.Get(r.Context(), appID)
	if err != nil || ra == nil || ra.Definition == nil || ra.Definition.Tools == nil {
		writeError(w, http.StatusNotFound, "not_found", "app not found")
		return
	}
	block, ok := ra.Definition.Tools.Modules[moduleID]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "module not declared by this app")
		return
	}
	man, hasMan := d.modules.Manifest(moduleID)
	if !hasMan || len(man.ConfigSchema) == 0 {
		writeError(w, http.StatusBadRequest, "no_schema", "module has no config schema")
		return
	}
	schema := man.ConfigSchema

	bundle := modulesettings.DeepMerge(block.Config, nil)
	prev := modulesettings.DeepMerge(bundle, d.moduleSettings.Deltas(r.Context(), userID, appID, moduleID))
	restoreSecrets(submitted, schema, prev)

	deltas := modulesettings.Diff(submitted, bundle)
	if err := d.moduleSettings.Set(r.Context(), userID, appID, moduleID, deltas); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	merged := modulesettings.DeepMerge(bundle, deltas)
	writeJSON(w, http.StatusOK, map[string]any{"value": redactSecrets(merged, schema)})
}

func restoreSecrets(submitted any, schema map[string]any, prev any) {
	if schema == nil {
		return
	}
	switch sv := submitted.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		pm, _ := prev.(map[string]any)
		for k, val := range sv {
			ps, ok := props[k].(map[string]any)
			if !ok {
				continue
			}
			if f, _ := ps["format"].(string); f == "password" {
				if s, ok := val.(string); ok && s == secretSentinel {
					if pm != nil {
						sv[k] = pm[k]
					} else {
						delete(sv, k)
					}
				}
				continue
			}
			var pv any
			if pm != nil {
				pv = pm[k]
			}
			restoreSecrets(val, ps, pv)
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		pa, _ := prev.([]any)
		for i, item := range sv {
			var pv any
			if i < len(pa) {
				pv = pa[i]
			}
			restoreSecrets(item, items, pv)
		}
	}
}

func deepCopyJSON(v map[string]any) any {
	if v == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func redactSecrets(v any, schema map[string]any) any {
	if v == nil || schema == nil {
		return v
	}
	if f, _ := schema["format"].(string); f == "password" {
		if s, ok := v.(string); ok && s != "" {
			return secretSentinel
		}
		return v
	}
	switch vv := v.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for k, val := range vv {
			if ps, ok := props[k].(map[string]any); ok {
				vv[k] = redactSecrets(val, ps)
			}
		}
		return vv
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range vv {
				vv[i] = redactSecrets(item, items)
			}
		}
		return vv
	}
	return v
}
