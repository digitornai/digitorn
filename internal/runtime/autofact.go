package runtime

import (
	"fmt"
	"strings"
)

// AutoFact extracts a short, structured fact from a tool call outcome that
// should be remembered across compactions. Returns "" when nothing noteworthy
// occurred. These facts feed snap.Facts (the lossless, never-compacted channel)
// so the agent always knows which files it modified and what commands it ran,
// even after many successive compactions — without relying on the LLM summarizer.
//
// Only write operations and meaningful commands are tracked. Reads are skipped
// (they don't change state). Errors are skipped (visible in the conversation).
func AutoFact(toolName string, args map[string]any, status string) string {
	if status == "errored" || status == "cancelled" {
		return ""
	}
	dot := strings.IndexByte(toolName, '.')
	if dot <= 0 || dot == len(toolName)-1 {
		return ""
	}
	module, action := toolName[:dot], toolName[dot+1:]

	switch module {
	case "filesystem":
		return filesystemFact(action, args)
	case "bash", "shell", "powershell":
		return bashFact(action, args)
	}
	return ""
}

func filesystemFact(action string, args map[string]any) string {
	switch action {
	case "write", "create":
		if path := strArg(args, "path"); path != "" {
			return fmt.Sprintf("[wrote] %s", path)
		}
	case "edit", "patch":
		if path := strArg(args, "path"); path != "" {
			return fmt.Sprintf("[edited] %s", path)
		}
	case "delete", "remove":
		if path := strArg(args, "path"); path != "" {
			return fmt.Sprintf("[deleted] %s", path)
		}
	case "move", "rename":
		src := strArg(args, "source")
		dst := strArg(args, "destination")
		if src == "" {
			src = strArg(args, "src")
		}
		if dst == "" {
			dst = strArg(args, "dst")
		}
		if src != "" && dst != "" {
			return fmt.Sprintf("[moved] %s → %s", src, dst)
		}
	case "mkdir":
		if path := strArg(args, "path"); path != "" {
			return fmt.Sprintf("[mkdir] %s", path)
		}
	}
	return ""
}

func bashFact(action string, args map[string]any) string {
	if action == "background_run" || action == "background_status" {
		return ""
	}
	cmd := strArg(args, "command")
	if cmd == "" {
		cmd = strArg(args, "cmd")
	}
	if cmd == "" {
		return ""
	}
	cmd = strings.TrimSpace(cmd)
	// Skip trivial read-only commands that don't change state.
	for _, trivial := range []string{"ls", "cat", "echo", "pwd", "which", "find", "grep",
		"head", "tail", "wc", "diff", "tree", "du", "df", "ps", "top", "date",
		"go test", "go build", "make test", "curl -s", "wget"} {
		if strings.HasPrefix(cmd, trivial) {
			return ""
		}
	}
	if len(cmd) > 120 {
		cmd = cmd[:117] + "…"
	}
	return fmt.Sprintf("[ran] %s", cmd)
}

func strArg(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
