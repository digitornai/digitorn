package server

import (
	"net/http"
	"regexp"
	"sort"

	"github.com/go-chi/chi/v5"
)

// channelSecretRe matches the `{{secret.NAME}}` placeholders an app's channel
// config uses to keep credentials (a bot token) out of the bundle. The user
// fills these in the background view; the background service resolves them from
// the per-user app-secret store at arm time.
var channelSecretRe = regexp.MustCompile(`\{\{\s*secret\.([A-Za-z0-9_]+)\s*\}\}`)

type channelSecretField struct {
	Key string `json:"key"`
	Set bool   `json:"set"`
}

type channelSecretsEntry struct {
	Provider string               `json:"provider"`
	Adapter  string               `json:"adapter"`
	Secrets  []channelSecretField `json:"secrets"`
}

// appChannelSecrets lists, per channel provider an app declares
// (tools.modules.channels.config.providers), the `{{secret.X}}` credentials it
// needs and whether the calling user has set each. The actual values are stored
// via the existing PUT /api/apps/{id}/secrets and never returned here.
func (d *Daemon) appChannelSecrets(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	uid := userIDOf(r.Context())

	ra, err := d.appMgr.Get(r.Context(), appID)
	if err != nil || ra == nil || ra.Definition == nil || ra.Definition.Tools == nil {
		writeJSON(w, http.StatusOK, map[string]any{"channels": []any{}, "count": 0})
		return
	}
	block, ok := ra.Definition.Tools.Modules["channels"]
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"channels": []any{}, "count": 0})
		return
	}
	providers, _ := block.Config["providers"].(map[string]any)

	store := d.ensureSecretStore()
	out := make([]channelSecretsEntry, 0, len(providers))
	for name, raw := range providers {
		pm, _ := raw.(map[string]any)
		if pm == nil {
			continue
		}
		adapter, _ := pm["adapter"].(string)
		keys := map[string]struct{}{}
		collectSecretKeys(pm["config"], keys)

		fields := make([]channelSecretField, 0, len(keys))
		for k := range keys {
			_, set := store.get(uid, appID, k)
			fields = append(fields, channelSecretField{Key: k, Set: set})
		}
		sort.Slice(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
		out = append(out, channelSecretsEntry{Provider: name, Adapter: adapter, Secrets: fields})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	writeJSON(w, http.StatusOK, map[string]any{"channels": out, "count": len(out)})
}

// appChannelSecretValue returns the installation's stored value for one channel
// secret key, so the background service can resolve a {{secret.X}} placeholder
// in an app's channel config at arm time (the UI-pasted token reaches the bot
// without an env var). Installation-scoped (getAny) because a channel bot token
// is shared per app, not per end-user. Returns {value:""} when unset.
func (d *Daemon) appChannelSecretValue(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	key := r.URL.Query().Get("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "key query param required")
		return
	}
	v, _ := d.ensureSecretStore().getAny(appID, key)
	if v == secretSentinel {
		v = "" // a corrupted/redacted echo is never a usable credential
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": v})
}

// resolveConfigSecrets deep-copies a channel config, replacing every
// `{{secret.X}}` placeholder with the installation's stored value for that key
// (left untouched when unset, so the background's env fallback still applies).
// The daemon resolves secrets here — before pushing the trigger to the
// background — so the bot token never has to round-trip back over the process
// boundary.
func (d *Daemon) resolveConfigSecrets(appID string, v any) any {
	store := d.ensureSecretStore()
	return resolveSecretPlaceholders(v, func(key string) (string, bool) {
		return store.getAny(appID, key)
	})
}

// collectSecretKeys walks a config value and records every `{{secret.X}}` key.
func collectSecretKeys(v any, into map[string]struct{}) {
	switch t := v.(type) {
	case string:
		for _, m := range channelSecretRe.FindAllStringSubmatch(t, -1) {
			into[m[1]] = struct{}{}
		}
	case map[string]any:
		for _, vv := range t {
			collectSecretKeys(vv, into)
		}
	case []any:
		for _, vv := range t {
			collectSecretKeys(vv, into)
		}
	}
}
