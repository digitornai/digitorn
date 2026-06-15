package rag

import (
	"reflect"
	"testing"
)

func TestResolveKBs(t *testing.T) {
	cfg := Config{
		DefaultKnowledgeBase: "",
		Sources: []SourceConfig{
			{KnowledgeBase: "docs"},
			{KnowledgeBase: "tickets"},
			{KnowledgeBase: "docs"}, // dup
			{KnowledgeBase: ""},     // → default
		},
	}
	cases := []struct {
		name      string
		cfg       Config
		requested string
		want      []string
	}{
		{"explicit wins", cfg, "tickets", []string{"tickets"}},
		{"default wins over sources", Config{DefaultKnowledgeBase: "main", Sources: cfg.Sources}, "", []string{"main"}},
		{"auto-route across all app KBs", cfg, "", []string{"docs", "tickets", "default"}},
		{"empty config falls back to default", Config{}, "", []string{"default"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveKBs(c.cfg, c.requested); !reflect.DeepEqual(got, c.want) {
				t.Errorf("resolveKBs(%q) = %v, want %v", c.requested, got, c.want)
			}
		})
	}
}
