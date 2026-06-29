package credentials

// Provider is one entry in the built-in provider catalogue that drives the
// "add a credential" form. It is purely descriptive — the daemon does not run
// per-handler validation or live tests here (that is deliberately out of scope
// for stability). Unknown providers are covered by the web's client-side custom
// templates, so this list only needs the common known ones.
type Provider struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Category    string  `json:"category"` // llm, database, vcs, …
	Type        string  `json:"type"`     // handler type: api_key, connection_string, …
	Icon        string  `json:"icon"`
	Description string  `json:"description,omitempty"`
	DocsURL     string  `json:"docs_url,omitempty"`
	Fields      []Field `json:"fields"`
	Verify      *Verify `json:"verify,omitempty"` // live-test recipe, when available
}

// Verify is a recipe to validate a credential against the live provider: an
// HTTP request whose status code tells us whether the key is accepted. Both the
// endpoint and the auth headers may template {field} values from the credential
// (e.g. {api_key}). AuthTemplate holds one "Header: value" per line.
type Verify struct {
	Endpoint     string `json:"endpoint"`
	Method       string `json:"method,omitempty"`        // default GET
	AuthTemplate string `json:"auth_template,omitempty"` // newline-separated "Header: value"
	SuccessCodes []int  `json:"success_codes,omitempty"` // default [200]
}

// Field is one form input for a provider credential.
type Field struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type"` // text, password, url, textarea
	Required    bool   `json:"required"`
	Masked      bool   `json:"masked,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	PrefixCheck string `json:"prefix_check,omitempty"`
	Help        string `json:"help,omitempty"`
}

func apiKeyField(placeholder, prefix string) Field {
	return Field{
		Name:        "api_key",
		Label:       "API key",
		Type:        "password",
		Required:    true,
		Masked:      true,
		Placeholder: placeholder,
		PrefixCheck: prefix,
	}
}

func connStringField(placeholder string) Field {
	return Field{
		Name:        "connection_string",
		Label:       "Connection string",
		Type:        "password",
		Required:    true,
		Masked:      true,
		Placeholder: placeholder,
	}
}

// verifyRecipes maps a provider id to its live-test recipe. Only providers with
// a cheap, reliable "list/whoami" endpoint are covered; the rest get no live
// test (the handler reports that gracefully). Method defaults to GET, success
// codes to [200].
var verifyRecipes = map[string]*Verify{
	"openai":     {Endpoint: "https://api.openai.com/v1/models", AuthTemplate: "Authorization: Bearer {api_key}"},
	"anthropic":  {Endpoint: "https://api.anthropic.com/v1/models", AuthTemplate: "x-api-key: {api_key}\nanthropic-version: 2023-06-01"},
	"google":     {Endpoint: "https://generativelanguage.googleapis.com/v1beta/models?key={api_key}"},
	"deepseek":   {Endpoint: "https://api.deepseek.com/models", AuthTemplate: "Authorization: Bearer {api_key}"},
	"mistral":    {Endpoint: "https://api.mistral.ai/v1/models", AuthTemplate: "Authorization: Bearer {api_key}"},
	"groq":       {Endpoint: "https://api.groq.com/openai/v1/models", AuthTemplate: "Authorization: Bearer {api_key}"},
	"openrouter": {Endpoint: "https://openrouter.ai/api/v1/key", AuthTemplate: "Authorization: Bearer {api_key}"},
	"github":     {Endpoint: "https://api.github.com/user", AuthTemplate: "Authorization: Bearer {api_key}"},
}

// Catalog returns the built-in provider catalogue, stable across calls.
func Catalog() []Provider {
	out := buildCatalog()
	for i := range out {
		if v, ok := verifyRecipes[out[i].ID]; ok {
			out[i].Verify = v
		}
	}
	return out
}

func buildCatalog() []Provider {
	return []Provider{
		{ID: "openai", DisplayName: "OpenAI", Category: "llm", Type: "api_key", Icon: "openai",
			Description: "GPT, embeddings, audio, vision.", DocsURL: "https://platform.openai.com/api-keys",
			Fields: []Field{apiKeyField("sk-proj-...", "sk-")}},
		{ID: "anthropic", DisplayName: "Anthropic", Category: "llm", Type: "api_key", Icon: "anthropic",
			Description: "Claude models.", DocsURL: "https://console.anthropic.com/settings/keys",
			Fields: []Field{apiKeyField("sk-ant-...", "sk-ant-")}},
		{ID: "google", DisplayName: "Google Gemini", Category: "llm", Type: "api_key", Icon: "google",
			Description: "Gemini models via AI Studio.", DocsURL: "https://aistudio.google.com/app/apikey",
			Fields: []Field{apiKeyField("AIza...", "")}},
		{ID: "deepseek", DisplayName: "DeepSeek", Category: "llm", Type: "api_key", Icon: "deepseek",
			Description: "DeepSeek chat & reasoning.", DocsURL: "https://platform.deepseek.com/api_keys",
			Fields: []Field{apiKeyField("sk-...", "sk-")}},
		{ID: "mistral", DisplayName: "Mistral", Category: "llm", Type: "api_key", Icon: "mistral",
			Description: "Mistral & Codestral models.", DocsURL: "https://console.mistral.ai/api-keys",
			Fields: []Field{apiKeyField("", "")}},
		{ID: "groq", DisplayName: "Groq", Category: "llm", Type: "api_key", Icon: "groq",
			Description: "Low-latency inference.", DocsURL: "https://console.groq.com/keys",
			Fields: []Field{apiKeyField("gsk_...", "gsk_")}},
		{ID: "openrouter", DisplayName: "OpenRouter", Category: "llm", Type: "api_key", Icon: "openrouter",
			Description: "Unified gateway to many models.", DocsURL: "https://openrouter.ai/keys",
			Fields: []Field{apiKeyField("sk-or-...", "sk-or-")}},
		{ID: "github", DisplayName: "GitHub", Category: "vcs", Type: "api_key", Icon: "github",
			Description: "Personal access token (repos, issues, actions).", DocsURL: "https://github.com/settings/tokens",
			Fields: []Field{{Name: "api_key", Label: "Personal access token", Type: "password", Required: true, Masked: true, Placeholder: "ghp_... / github_pat_..."}}},
		{ID: "github_copilot", DisplayName: "GitHub Copilot", Category: "llm", Type: "device_code", Icon: "github",
			Description: "Use your GitHub Copilot subscription as an LLM backend. Sign in with GitHub (device flow) — no key to paste.",
			DocsURL:     "https://github.com/settings/copilot",
			Fields:      []Field{}},
		{ID: "postgres", DisplayName: "PostgreSQL", Category: "database", Type: "connection_string", Icon: "postgres",
			Description: "Standard libpq URI.",
			Fields:      []Field{connStringField("postgres://user:pass@host:5432/db")}},
		{ID: "mysql", DisplayName: "MySQL", Category: "database", Type: "connection_string", Icon: "mysql",
			Description: "MySQL / MariaDB connection URL.",
			Fields:      []Field{connStringField("mysql://user:pass@host:3306/db")}},
		{ID: "mongodb", DisplayName: "MongoDB", Category: "database", Type: "connection_string", Icon: "mongodb",
			Description: "MongoDB connection URI.",
			Fields:      []Field{connStringField("mongodb+srv://user:pass@cluster/db")}},
		{ID: "redis", DisplayName: "Redis", Category: "database", Type: "connection_string", Icon: "redis",
			Description: "Redis connection URL.",
			Fields:      []Field{connStringField("redis://:pass@host:6379/0")}},
	}
}
