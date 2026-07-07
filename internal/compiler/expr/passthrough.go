package expr

// RuntimeNamespaces are placeholder roots resolved by the runtime, never the
// compiler. Their occurrences pass through unchanged so the runtime can fill
// them in at turn time.
var RuntimeNamespaces = []string{
	"event", "events",
	"caller", "client",
	"state",
	"message", "msg",
	"turn",
	"session",
	"user",
	"result", "results",
	"params", "param",
	"input", "output", "outputs",
	"tool",
	"agent",
	"context", "ctx",
	"workspace", "ws",
	"steps", "step",
	"field",
	"previous",
	"approvals",
	"tasks",
	"error",
}

func isRuntimeNamespace(ns string) bool {
	for _, r := range RuntimeNamespaces {
		if ns == r {
			return true
		}
	}
	return false
}
