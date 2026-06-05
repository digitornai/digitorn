package catalog

func defaultMiddleware() []string {
	return []string{
		"mask_secrets",
		"prompt_inject",
		"content_filter",
		"rag_inject",
		"response_filter",
		"retry",
		"timeout",
		"circuit_breaker",
		"audit",
		"dedup",
		"semantic_cache",
		"auto_heal",
		"cross_context",
		"budget",
	}
}
