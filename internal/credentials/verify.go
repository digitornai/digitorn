package credentials

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/dbaccess"
)

// testTimeout bounds a live verification so a slow/unreachable provider can't
// hang the settings request. This runs on the settings plane (its own request
// goroutine), never on an agent's hot path.
const testTimeout = 10 * time.Second

// TestResult is the outcome of a live credential verification.
type TestResult struct {
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
	LatencyMS int64  `json:"latency_ms"`
}

// RunTest validates the supplied fields against the provider's live endpoint.
// It looks up a recipe from the catalogue; for an unknown provider that ships a
// base_url + api_key (custom LLM), it falls back to GET {base_url}/models. When
// no recipe applies it reports that a live test isn't available (OK=false) —
// the caller distinguishes "not available" from "rejected" via Detail.
func RunTest(ctx context.Context, providerName string, fields map[string]string) TestResult {
	fields = trimValues(fields)
	// Database credentials carry a connection string — verify by actually
	// connecting (and authenticating) via the shared dbaccess layer.
	if cs := fields["connection_string"]; cs != "" {
		return verifyDB(ctx, cs)
	}
	v, ok := lookupVerify(providerName, fields)
	if !ok {
		return TestResult{OK: false, Detail: "Live test not available for this provider."}
	}

	method := v.Method
	if method == "" {
		method = http.MethodGet
	}
	endpoint := subst(v.Endpoint, fields)

	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return TestResult{OK: false, Detail: "Invalid endpoint: " + err.Error()}
	}
	req.Header.Set("User-Agent", "digitorn-daemon")
	req.Header.Set("Accept", "application/json")
	for _, line := range strings.Split(v.AuthTemplate, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		req.Header.Set(strings.TrimSpace(name), subst(strings.TrimSpace(val), fields))
	}

	start := time.Now()
	resp, err := (&http.Client{Timeout: testTimeout}).Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return TestResult{OK: false, Detail: "Could not reach the provider: " + err.Error(), LatencyMS: latency}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	codes := v.SuccessCodes
	if len(codes) == 0 {
		codes = []int{200}
	}
	for _, c := range codes {
		if resp.StatusCode == c {
			return TestResult{OK: true, Detail: fmt.Sprintf("Valid — provider accepted the credential (HTTP %d).", resp.StatusCode), LatencyMS: latency}
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return TestResult{OK: false, Detail: fmt.Sprintf("Provider rejected the credential (HTTP %d).", resp.StatusCode), LatencyMS: latency}
	}
	return TestResult{OK: false, Detail: fmt.Sprintf("Unexpected response from provider (HTTP %d).", resp.StatusCode), LatencyMS: latency}
}

// verifyDB validates a database connection string by opening a real connection
// through the shared dbaccess layer (which pings/authenticates on Open). Egress
// is "open" so the user can test their own local/private database — same trust
// model as the HTTP api_key test that dials arbitrary hosts.
func verifyDB(ctx context.Context, connStr string) TestResult {
	kind := dbKindFromDSN(connStr)
	if kind == "" {
		return TestResult{OK: false, Detail: "Unsupported database URL — expected postgres://, mysql://, mongodb:// or redis://."}
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	start := time.Now()
	db, err := dbaccess.Open(ctx, dbaccess.ConnConfig{
		Kind:     kind,
		DSN:      connStr,
		Security: dbaccess.SecurityPolicy{Mode: "read_only", Egress: "open"},
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return TestResult{OK: false, Detail: dbErrDetail(err), LatencyMS: latency}
	}
	_ = db.Close()
	return TestResult{OK: true, Detail: "Valid — connected to " + kind + ".", LatencyMS: latency}
}

// dbErrDetail turns a driver's verbose (often multi-line, duplicated) error
// into one clear sentence for the UI.
func dbErrDetail(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return "Host unreachable — connection refused (check host/port)."
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "lookup"):
		return "Host not found — check the hostname."
	case strings.Contains(msg, "password authentication failed"),
		strings.Contains(msg, "access denied"),
		strings.Contains(msg, "authentication failed"),
		strings.Contains(msg, "auth"):
		return "Authentication failed — check user/password."
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "Connection timed out — host unreachable or firewalled."
	case strings.Contains(msg, "does not exist"), strings.Contains(msg, "unknown database"):
		return "Database not found — check the database name."
	}
	// Fallback: first line only, prefixes stripped.
	first := strings.TrimSpace(strings.SplitN(err.Error(), "\n", 2)[0])
	if i := strings.LastIndex(first, ": "); i >= 0 && i < len(first)-2 {
		first = strings.TrimSpace(first[i+2:])
	}
	return "Connection failed: " + first
}

func dbKindFromDSN(dsn string) string {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return ""
	}
	switch strings.ToLower(dsn[:i]) {
	case "postgres", "postgresql", "pg":
		return "postgres"
	case "mysql", "mariadb":
		return "mysql"
	case "mongodb", "mongodb+srv":
		return "mongodb"
	case "redis", "rediss":
		return "redis"
	}
	return ""
}

func lookupVerify(providerName string, fields map[string]string) (*Verify, bool) {
	if v, ok := verifyRecipes[providerName]; ok {
		return v, true
	}
	// Custom OpenAI-compatible endpoint stored as base_url + api_key.
	if bu := strings.TrimSpace(fields["base_url"]); bu != "" && fields["api_key"] != "" {
		return &Verify{
			Endpoint:     strings.TrimRight(bu, "/") + "/models",
			AuthTemplate: "Authorization: Bearer {api_key}",
		}, true
	}
	return nil, false
}

// subst replaces {field} placeholders with the credential's field values.
func subst(tmpl string, fields map[string]string) string {
	if !strings.Contains(tmpl, "{") {
		return tmpl
	}
	out := tmpl
	for k, v := range fields {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}
