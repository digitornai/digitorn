package credentials

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

// LLMModel is one model a stored credential's key unlocks (BYOK). Shaped to map
// straight onto the web's GatewayModel: `owned_by` is the provider slug the
// model picker groups by.
type LLMModel struct {
	ID               string `json:"id"`
	OwnedBy          string `json:"owned_by"`
	Kind             string `json:"kind"`
	MaxContextTokens int    `json:"max_context_tokens,omitempty"`
}

// modelsRecipe describes how to list a provider's models from its API.
type modelsRecipe struct {
	endpoint     string // may template {api_key}
	authTemplate string // newline-separated "Header: value", may template {api_key}
	parser       string // "openai" ({data:[{id}]}) | "google" ({models:[{name}]})
}

// modelsRecipes covers the catalogue's LLM providers. (Copilot is handled
// separately via the device-flow token exchange; github PATs aren't LLMs.)
var modelsRecipes = map[string]modelsRecipe{
	"openai":     {endpoint: "https://api.openai.com/v1/models", authTemplate: "Authorization: Bearer {api_key}", parser: "openai"},
	"anthropic":  {endpoint: "https://api.anthropic.com/v1/models", authTemplate: "x-api-key: {api_key}\nanthropic-version: 2023-06-01", parser: "openai"},
	"google":     {endpoint: "https://generativelanguage.googleapis.com/v1beta/models?key={api_key}", parser: "google"},
	"deepseek":   {endpoint: "https://api.deepseek.com/models", authTemplate: "Authorization: Bearer {api_key}", parser: "openai"},
	"mistral":    {endpoint: "https://api.mistral.ai/v1/models", authTemplate: "Authorization: Bearer {api_key}", parser: "openai"},
	"groq":       {endpoint: "https://api.groq.com/openai/v1/models", authTemplate: "Authorization: Bearer {api_key}", parser: "openai"},
	"openrouter": {endpoint: "https://openrouter.ai/api/v1/models", authTemplate: "Authorization: Bearer {api_key}", parser: "openai"},
}

const modelsCacheTTL = 10 * time.Minute

type modelsCacheEntry struct {
	models    []LLMModel
	fetchedAt time.Time
}

// modelsCache is a short-lived per-credential cache so opening the picker
// repeatedly doesn't hammer the providers. This is on the SETTINGS/picker
// plane, never the agent hot path.
type modelsCache struct {
	mu   sync.Mutex
	byID map[string]modelsCacheEntry
	http *http.Client
}

func newModelsCache() *modelsCache {
	return &modelsCache{byID: map[string]modelsCacheEntry{}, http: &http.Client{Timeout: 12 * time.Second}}
}

// ListUserModels returns every model the user's stored LLM credentials unlock,
// best-effort: a credential whose key is rejected or has no model endpoint just
// contributes nothing. Results are deduped + sorted, grouped by provider via
// each model's OwnedBy = the credential's provider_name.
func (s *Store) ListUserModels(ctx context.Context, userID string) []LLMModel {
	rows, err := s.listRows(ctx, userID)
	if err != nil {
		return nil
	}
	out := []LLMModel{}
	seen := map[string]bool{} // provider|id
	for _, row := range rows {
		models := s.modelsForCredential(ctx, row)
		for _, m := range models {
			key := row.ProviderName + "|" + m.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OwnedBy != out[j].OwnedBy {
			return out[i].OwnedBy < out[j].OwnedBy
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Store) modelsForCredential(ctx context.Context, row models.UserCredential) []LLMModel {
	// Cache hit?
	s.models.mu.Lock()
	if e, ok := s.models.byID[row.ID]; ok && time.Since(e.fetchedAt) < modelsCacheTTL {
		s.models.mu.Unlock()
		return e.models
	}
	s.models.mu.Unlock()

	fields, err := s.openFields(row.Sealed)
	if err != nil {
		return nil
	}

	var models []LLMModel
	switch {
	case row.ProviderName == copilotProvider:
		models = s.copilotModels(ctx, fields["api_key"])
	default:
		models = s.fetchProviderModels(ctx, row.ProviderName, fields)
	}

	s.models.mu.Lock()
	s.models.byID[row.ID] = modelsCacheEntry{models: models, fetchedAt: time.Now()}
	s.models.mu.Unlock()
	return models
}

func (s *Store) copilotModels(ctx context.Context, ghToken string) []LLMModel {
	if ghToken == "" {
		return nil
	}
	tok, err := s.exchangeCopilotToken(ctx, ghToken)
	if err != nil {
		return nil
	}
	cms, err := s.fetchCopilotModels(ctx, tok)
	if err != nil {
		return nil
	}
	out := make([]LLMModel, 0, len(cms))
	for _, m := range cms {
		cw := 0
		if m.ContextWindow != nil {
			cw = *m.ContextWindow
		}
		out = append(out, LLMModel{ID: m.ID, OwnedBy: copilotProvider, Kind: "chat", MaxContextTokens: cw})
	}
	return out
}

func (s *Store) fetchProviderModels(ctx context.Context, provider string, fields map[string]string) []LLMModel {
	rec, ok := modelsRecipes[provider]
	if !ok {
		// Custom OpenAI-compatible endpoint stored as base_url + api_key.
		if bu := strings.TrimSpace(fields["base_url"]); bu != "" && fields["api_key"] != "" {
			rec = modelsRecipe{endpoint: strings.TrimRight(bu, "/") + "/models", authTemplate: "Authorization: Bearer {api_key}", parser: "openai"}
		} else {
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, subst(rec.endpoint, fields), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "digitorn-daemon")
	for _, line := range strings.Split(rec.authTemplate, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if name, val, found := strings.Cut(line, ":"); found {
			req.Header.Set(strings.TrimSpace(name), subst(strings.TrimSpace(val), fields))
		}
	}
	resp, err := s.models.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil
	}
	if rec.parser == "google" {
		return parseGoogleModels(raw, provider)
	}
	return parseOpenAIModels(raw, provider)
}

func parseOpenAIModels(raw []byte, provider string) []LLMModel {
	var d struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil
	}
	out := make([]LLMModel, 0, len(d.Data))
	for _, m := range d.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, LLMModel{ID: m.ID, OwnedBy: provider, Kind: "chat"})
	}
	return out
}

func parseGoogleModels(raw []byte, provider string) []LLMModel {
	var d struct {
		Models []struct {
			Name                       string   `json:"name"` // "models/gemini-1.5-pro"
			InputTokenLimit            int      `json:"inputTokenLimit"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil
	}
	out := make([]LLMModel, 0, len(d.Models))
	for _, m := range d.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if id == "" {
			continue
		}
		// Only chat/generateContent-capable models.
		chat := len(m.SupportedGenerationMethods) == 0
		for _, g := range m.SupportedGenerationMethods {
			if g == "generateContent" {
				chat = true
			}
		}
		if !chat {
			continue
		}
		out = append(out, LLMModel{ID: id, OwnedBy: provider, Kind: "chat", MaxContextTokens: m.InputTokenLimit})
	}
	return out
}
