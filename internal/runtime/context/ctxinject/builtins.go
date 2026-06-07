package ctxinject

import (
	"fmt"
	"strings"

	"github.com/mbathepaul/digitorn/internal/runtime/context/repomap"
)

// Builtin renders a ready-made section body from the turn data. Returns "" to skip
// (e.g. no data to show).
type Builtin func(d Data) string

// builtins are the pre-configured, ready-to-use sections an app references by name
// (`builtin: datetime`). Domain-agnostic — they draw only from the generic data bag.
var builtins = map[string]Builtin{
	"datetime":    biDatetime,
	"date":        biDatetime,
	"user":        biUser,
	"session":     biSession,
	"identity":    biIdentity,
	"environment": biEnv,
	"env":         biEnv,
	"code_index":  biCodeIndex,
}

// BuiltinNames lists the available builtins (for validation / docs).
func BuiltinNames() []string {
	return []string{"datetime", "user", "session", "identity", "environment", "code_index"}
}

func biCodeIndex(d Data) string {
	root := toString(d.Session["workdir"])
	if root == "" {
		return ""
	}
	return repomap.Get(root)
}

func biDatetime(d Data) string {
	if d.Now.IsZero() {
		return ""
	}
	return fmt.Sprintf("Current date: %s (%s), local time %s. This is a snapshot taken at the start of the turn — it does not advance while you work.",
		d.Now.Format("2006-01-02"), d.Now.Weekday().String(), d.Now.Format("15:04"))
}

func biUser(d Data) string {
	get := func(k string) string { return toString(d.User[k]) }
	name := get("name")
	if name == "" {
		name = get("id")
	}
	if name == "" && len(d.User) == 0 {
		return ""
	}
	var b strings.Builder
	if name != "" {
		fmt.Fprintf(&b, "You are assisting %s.", name)
	} else {
		b.WriteString("You are assisting the user.")
	}
	if v := get("region"); v != "" {
		fmt.Fprintf(&b, " Region: %s.", v)
	}
	if v := get("locale"); v != "" {
		fmt.Fprintf(&b, " Locale: %s.", v)
	}
	if v := get("timezone"); v != "" {
		fmt.Fprintf(&b, " Timezone: %s.", v)
	}
	if v := get("email"); v != "" {
		fmt.Fprintf(&b, " Email: %s.", v)
	}
	if v := toString(d.User["roles"]); v != "" {
		fmt.Fprintf(&b, " Roles: %s.", v)
	}
	return b.String()
}

func biSession(d Data) string {
	get := func(k string) string { return toString(d.Session[k]) }
	var lines []string
	if v := get("goal"); v != "" {
		lines = append(lines, "Goal: "+v)
	}
	if v := get("mode"); v != "" {
		lines = append(lines, "Mode: "+v)
	}
	if v := get("turn"); v != "" && v != "0" {
		lines = append(lines, "Turn: "+v)
	}
	if v := get("workdir"); v != "" {
		lines = append(lines, "Working directory: "+v)
	}
	return strings.Join(lines, "\n")
}

func biEnv(d Data) string {
	get := func(k string) string { return toString(d.Env[k]) }
	platform, os, arch := get("platform"), get("os"), get("arch")
	if platform == "" && os == "" {
		return ""
	}
	var parts []string
	if platform != "" {
		parts = append(parts, "Platform: "+platform)
	}
	if os != "" {
		parts = append(parts, "OS: "+os)
	}
	if arch != "" {
		parts = append(parts, "Arch: "+arch)
	}
	if v := get("shell"); v != "" {
		parts = append(parts, "Shell: "+v)
	}
	return strings.Join(parts, " · ")
}

func biIdentity(d Data) string {
	app := toString(d.App["name"])
	if app == "" {
		app = toString(d.App["id"])
	}
	if app == "" {
		return ""
	}
	out := "You are running inside the " + app + " application."
	if v := toString(d.App["version"]); v != "" {
		out += " (version " + v + ")"
	}
	return out
}
