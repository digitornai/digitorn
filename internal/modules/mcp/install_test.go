package mcp

import (
	"strings"
	"testing"
)

func TestValidatePackage(t *testing.T) {
	valid := []string{"mcp-server-fetch", "mcp_server.git", "package123"}
	for _, p := range valid {
		if err := validatePackage(p); err != nil {
			t.Errorf("validatePackage(%q) unexpected error: %v", p, err)
		}
	}
	invalid := []string{
		"",
		"pkg with space",
		"pkg --index-url http://evil",
		"pkg;whoami",
		"@scope/pkg",
		strings.Repeat("a", 101),
	}
	for _, p := range invalid {
		if err := validatePackage(p); err == nil {
			t.Errorf("validatePackage(%q) should have failed", p)
		}
	}
}
