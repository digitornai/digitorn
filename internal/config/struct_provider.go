package config

import (
	"github.com/knadh/koanf/v2"
)

// structProvider is a minimal koanf provider that loads from a struct via
// koanf's built-in raw-map loader.
func structProvider(in any) koanf.Provider {
	return &structSrc{src: in}
}

type structSrc struct{ src any }

func (s *structSrc) ReadBytes() ([]byte, error) { return nil, nil }
func (s *structSrc) Read() (map[string]any, error) {
	// Use a JSON round-trip to flatten the struct into a map (simple,
	// dependency-free, and respects koanf tags via Unmarshal).
	return structToMap(s.src)
}
