package background

import "testing"

func TestDetectLocalServerURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"vite", "  ➜  Local:   http://localhost:5173/", "http://localhost:5173/"},
		{"next", "- ready started server on http://localhost:3000", "http://localhost:3000/"},
		{"127.0.0.1", "Server running at http://127.0.0.1:8080/", "http://127.0.0.1:8080/"},
		{"0.0.0.0 rewritten", "Listening on http://0.0.0.0:4000", "http://localhost:4000/"},
		{"keeps path", "open http://localhost:3000/dashboard now", "http://localhost:3000/dashboard"},
		{"ansi wrapped", "\x1b[32m➜\x1b[0m Local: \x1b[36mhttp://localhost:5173/\x1b[0m", "http://localhost:5173/"},
		{"no url", "npm install: added 210 packages", ""},
		{"non-loopback ignored", "deploying to http://example.com:8080", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectLocalServerURL(c.in); got != c.want {
				t.Fatalf("detectLocalServerURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
