package mcp

const argAppend = "__ARG_APPEND__"

var standardKeys = map[string]bool{
	"transport": true, "command": true, "args": true, "env": true, "url": true,
	"headers": true, "timeout": true, "buffer_size": true, "auth": true,
	"examples": true, "rate_limit_rpm": true, "via": true,
	"smithery_key": true, "smithery_namespace": true, "smithery_slug": true,
}

const (
	smitheryConnectBase = "https://api.smithery.ai/connect"
	smitheryProxyBase   = "https://server.smithery.ai"
)

var smitherySlugs = map[string]string{
	"github":              "@smithery-ai/github",
	"slack":               "@smithery-ai/slack",
	"fetch":               "@smithery-ai/fetch",
	"brave_search":        "@anthropics/brave-web-search",
	"filesystem":          "@anthropics/filesystem",
	"memory":              "@anthropics/memory",
	"puppeteer":           "@anthropics/puppeteer",
	"sequential_thinking": "@anthropics/sequential-thinking",
	"everart":             "@anthropics/everart",
	"google_maps":         "@anthropics/google-maps",
	"postgres":            "@anthropics/postgres",
	"brave":               "@anthropics/brave-web-search",
}

type catalogEntry struct {
	DisplayName      string
	Description      string
	Transport        string
	Command          string
	Args             []string
	Runtime          string
	Package          string
	EnvMapping       map[string]string
	DefaultEnv       map[string]string
	OAuthProvider    string
	OAuthEnvTokenVar string
	OAuthScopes      []string
	OAuthStyle       string
	// google_keyfile style: the env vars the server reads for its OAuth client
	// keyfile + credentials file, and the credentials filename it writes.
	OAuthKeyfileEnv          string
	OAuthCredentialsEnv      string
	OAuthCredentialsFilename string
	SmitherySlug             string
	BinaryName               string
	Timeout                  float64
}

func catalogLookup(id string) (catalogEntry, bool) {
	e, ok := catalog[id]
	return e, ok
}

var catalog = map[string]catalogEntry{
	"github": {
		DisplayName: "GitHub", Description: "GitHub API (repos, issues, PRs, search, code)",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-github"},
		Package:    "@modelcontextprotocol/server-github",
		EnvMapping: map[string]string{"token": "GITHUB_PERSONAL_ACCESS_TOKEN"},
	},
	"notion": {
		DisplayName: "Notion", Description: "Notion workspace (pages, databases, search)",
		Command: "mcp-notion", Runtime: "pip", Package: "mcp-notion",
		EnvMapping:    map[string]string{"token": "NOTION_API_KEY"},
		OAuthProvider: "notion", OAuthStyle: "env_token", OAuthEnvTokenVar: "NOTION_API_KEY",
	},
	"slack": {
		DisplayName: "Slack", Description: "Slack workspace (channels, messages, users)",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-slack"},
		Package: "@modelcontextprotocol/server-slack",
		EnvMapping: map[string]string{
			"bot_token": "SLACK_BOT_TOKEN", "token": "SLACK_BOT_TOKEN", "team_id": "SLACK_TEAM_ID",
		},
	},
	"linear": {
		DisplayName: "Linear", Description: "Linear issue tracker (issues, projects, teams)",
		Command: "npx", Args: []string{"-y", "mcp-linear"}, Package: "mcp-linear",
		EnvMapping: map[string]string{"api_key": "LINEAR_API_KEY"},
	},
	"clickup": {
		DisplayName: "ClickUp", Description: "ClickUp project management (tasks, lists, spaces)",
		Command: "npx", Args: []string{"-y", "mcp-clickup"}, Package: "mcp-clickup",
		EnvMapping: map[string]string{"api_key": "CLICKUP_API_KEY"},
	},
	"atlassian": {
		DisplayName: "Atlassian (Jira/Confluence)", Description: "Jira issues and Confluence pages",
		Command: "npx", Args: []string{"-y", "mcp-atlassian"}, Package: "mcp-atlassian",
		EnvMapping: map[string]string{
			"site_url": "ATLASSIAN_URL", "email": "ATLASSIAN_EMAIL", "api_token": "ATLASSIAN_API_TOKEN",
		},
	},
	"google_drive": {
		DisplayName: "Google Drive", Description: "Google Drive file access",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-gdrive"},
		Package:       "@modelcontextprotocol/server-gdrive",
		OAuthProvider: "google", OAuthStyle: "google_keyfile",
		OAuthKeyfileEnv: "GDRIVE_OAUTH_PATH", OAuthCredentialsEnv: "GDRIVE_CREDENTIALS_PATH",
		OAuthCredentialsFilename: ".gdrive-server-credentials.json",
		OAuthScopes:              []string{"https://www.googleapis.com/auth/drive.readonly"},
	},
	"google_calendar": {
		DisplayName: "Google Calendar", Description: "Google Calendar events and scheduling",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-google-calendar"},
		Package:       "@modelcontextprotocol/server-google-calendar",
		OAuthProvider: "google", OAuthStyle: "google_keyfile",
		OAuthKeyfileEnv: "GDRIVE_OAUTH_PATH", OAuthCredentialsEnv: "GDRIVE_CREDENTIALS_PATH",
		OAuthCredentialsFilename: ".gdrive-server-credentials.json",
		OAuthScopes: []string{
			"https://www.googleapis.com/auth/calendar.readonly",
			"https://www.googleapis.com/auth/calendar.events",
		},
	},
	"gmail": {
		DisplayName: "Gmail", Description: "Gmail email access (read, send, search) with auto OAuth",
		Command: "npx", Args: []string{"-y", "@gongrzhe/server-gmail-autoauth-mcp"},
		Package: "@gongrzhe/server-gmail-autoauth-mcp", BinaryName: "gmail-mcp",
		OAuthProvider: "google", OAuthStyle: "google_keyfile",
		OAuthKeyfileEnv: "GMAIL_OAUTH_PATH", OAuthCredentialsEnv: "GMAIL_CREDENTIALS_PATH",
		OAuthCredentialsFilename: "credentials.json",
		OAuthScopes: []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/gmail.send",
		},
	},
	"google_maps": {
		DisplayName: "Google Maps", Description: "Google Maps geocoding, directions, places",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-google-maps"},
		Package:    "@modelcontextprotocol/server-google-maps",
		EnvMapping: map[string]string{"api_key": "GOOGLE_MAPS_API_KEY"},
	},
	"stripe": {
		DisplayName: "Stripe", Description: "Stripe payments (customers, invoices, subscriptions)",
		Command: "npx", Args: []string{"-y", "@stripe/mcp-server-stripe"},
		Package:    "@stripe/mcp-server-stripe",
		EnvMapping: map[string]string{"api_key": "STRIPE_API_KEY"},
	},
	"shopify": {
		DisplayName: "Shopify", Description: "Shopify store management (products, orders)",
		Command: "npx", Args: []string{"-y", "@anthropic/mcp-server-shopify"},
		Package:    "@anthropic/mcp-server-shopify",
		EnvMapping: map[string]string{"token": "SHOPIFY_ACCESS_TOKEN"},
	},
	"paypal": {
		DisplayName: "PayPal", Description: "PayPal payments and transactions",
		Command: "npx", Args: []string{"-y", "@anthropic/mcp-server-paypal"},
		Package: "@anthropic/mcp-server-paypal",
		EnvMapping: map[string]string{
			"client_id": "PAYPAL_CLIENT_ID", "client_secret": "PAYPAL_CLIENT_SECRET",
		},
	},
	"mailgun": {
		DisplayName: "Mailgun", Description: "Mailgun email sending and tracking",
		Command: "npx", Args: []string{"-y", "mcp-mailgun"}, Package: "mcp-mailgun",
		EnvMapping: map[string]string{"api_key": "MAILGUN_API_KEY", "domain": "MAILGUN_DOMAIN"},
	},
	"brave_search": {
		DisplayName: "Brave Search", Description: "Web search via Brave Search API",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-brave-search"},
		Package:    "@modelcontextprotocol/server-brave-search",
		EnvMapping: map[string]string{"api_key": "BRAVE_API_KEY"},
	},
	"fetch": {
		DisplayName: "Fetch", Description: "Fetch and extract content from web URLs",
		Command: "mcp-server-fetch", Runtime: "pip", Package: "mcp-server-fetch",
	},
	"puppeteer": {
		DisplayName: "Puppeteer", Description: "Browser automation via Puppeteer",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-puppeteer"},
		Package: "@modelcontextprotocol/server-puppeteer",
	},
	"postgres": {
		DisplayName: "PostgreSQL", Description: "PostgreSQL database access",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-postgres"},
		Package:    "@modelcontextprotocol/server-postgres",
		EnvMapping: map[string]string{"connection_string": argAppend},
	},
	"sqlite": {
		DisplayName: "SQLite", Description: "SQLite database access",
		Command: "mcp-server-sqlite", Args: []string{"--db-path"},
		Runtime: "pip", Package: "mcp-server-sqlite",
		EnvMapping: map[string]string{"database": argAppend},
	},
	"qdrant": {
		DisplayName: "Qdrant", Description: "Qdrant vector database",
		Command: "npx", Args: []string{"-y", "mcp-qdrant"}, Package: "mcp-qdrant",
		EnvMapping: map[string]string{"url": "QDRANT_URL", "api_key": "QDRANT_API_KEY"},
	},
	"filesystem": {
		DisplayName: "Filesystem", Description: "Local filesystem read/write access",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem"},
		Package:    "@modelcontextprotocol/server-filesystem",
		EnvMapping: map[string]string{"path": argAppend},
	},
	"memory": {
		DisplayName: "Memory (Knowledge Graph)", Description: "Persistent knowledge graph memory",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-memory"},
		Package: "@modelcontextprotocol/server-memory",
	},
	"git": {
		DisplayName: "Git", Description: "Git repository operations (log, diff, branch, commit)",
		Command: "mcp-server-git", Args: []string{"--repository"},
		Runtime: "pip", Package: "mcp-server-git",
		EnvMapping: map[string]string{"repository": argAppend},
	},
	"docker": {
		DisplayName: "Docker", Description: "Docker container management",
		Command: "npx", Args: []string{"-y", "mcp-docker"}, Package: "mcp-docker",
	},
	"sequential_thinking": {
		DisplayName: "Sequential Thinking", Description: "Step-by-step reasoning and problem decomposition",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-sequential-thinking"},
		Package: "@modelcontextprotocol/server-sequential-thinking",
	},
	"vercel": {
		DisplayName: "Vercel", Description: "Vercel deployments and project management",
		Command: "npx", Args: []string{"-y", "mcp-vercel"}, Package: "mcp-vercel",
		EnvMapping: map[string]string{"token": "VERCEL_TOKEN"},
	},
	"cloudflare": {
		DisplayName: "Cloudflare", Description: "Cloudflare Workers, DNS, and CDN management",
		Command: "npx", Args: []string{"-y", "mcp-cloudflare"}, Package: "mcp-cloudflare",
		EnvMapping: map[string]string{"api_token": "CLOUDFLARE_API_TOKEN"},
	},
	"aws": {
		DisplayName: "AWS", Description: "AWS services (S3, Lambda, EC2, etc.)",
		Command: "npx", Args: []string{"-y", "mcp-aws"}, Package: "mcp-aws",
		EnvMapping: map[string]string{
			"access_key_id": "AWS_ACCESS_KEY_ID", "secret_access_key": "AWS_SECRET_ACCESS_KEY",
			"region": "AWS_DEFAULT_REGION",
		},
	},
	"kubernetes": {
		DisplayName: "Kubernetes", Description: "Kubernetes cluster management",
		Command: "npx", Args: []string{"-y", "mcp-kubernetes"}, Package: "mcp-kubernetes",
		EnvMapping: map[string]string{"kubeconfig": "KUBECONFIG"},
	},
	"everart": {
		DisplayName: "EverArt", Description: "AI image generation via EverArt",
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-everart"},
		Package:    "@modelcontextprotocol/server-everart",
		EnvMapping: map[string]string{"api_key": "EVERART_API_KEY"},
	},
	"apify": {
		DisplayName: "Apify", Description: "Apify web scraping and automation actors",
		Command: "npx", Args: []string{"-y", "mcp-apify"}, Package: "mcp-apify",
		EnvMapping: map[string]string{"token": "APIFY_TOKEN"},
	},
}
