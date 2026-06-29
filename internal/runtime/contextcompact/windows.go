package contextcompact

// DefaultContextWindow is the absolute last-resort fallback, used ONLY when the
// gateway catalog (authoritative model window), the app's explicit
// context.max_tokens, and the app-level runtime.context.max_tokens all provide
// nothing. There is deliberately NO hardcoded per-model table: a model's window
// comes from the gateway or the app YAML. The value is kept large enough not to
// silently cripple a capable model when the chain falls through here.
const DefaultContextWindow = 32768
