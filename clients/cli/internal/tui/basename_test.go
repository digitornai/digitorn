package tui

import "testing"

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		`C:\Users\ASUS\.digitorn\workdirs\chat-claude\hash\sess\src\App.jsx`: "App.jsx",
		"my-app/my-react-app/src/components/Header.jsx":                      "Header.jsx",
		"package.json":      "package.json",
		`src\components\`:    "components",
		"":                  "",
	}
	for in, want := range cases {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}
