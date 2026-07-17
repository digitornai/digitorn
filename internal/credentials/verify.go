package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/dbaccess"
)

const testTimeout = 10 * time.Second

type TestResult struct {
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
	LatencyMS int64  `json:"latency_ms"`
}

func RunTest(ctx context.Context, providerName string, fields map[string]string) TestResult {
	fields = trimValues(fields)
	if cs := fields["connection_string"]; cs != "" {
		return verifyDB(ctx, cs)
	}
	if _, known := verifyRecipes[providerName]; !known {
		if base := strings.TrimRight(strings.TrimSpace(fields["base_url"]), "/"); base != "" {
			return verifyOpenAICompatible(ctx, base, strings.TrimSpace(fields["api_key"]))
		}
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

func verifyOpenAICompatible(ctx context.Context, base, apiKey string) TestResult {
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()

	try := func(url string) (status, models int, err error) {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if e != nil {
			return 0, 0, e
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "digitorn-daemon")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, e := (&http.Client{Timeout: testTimeout}).Do(req)
		if e != nil {
			return 0, 0, e
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var d struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &d)
		n := 0
		for _, m := range d.Data {
			if m.ID != "" {
				n++
			}
		}
		return resp.StatusCode, n, nil
	}

	endpoints := []string{base + "/models"}
	if !strings.Contains(base, "/v1") {
		endpoints = append(endpoints, base+"/v1/models")
	}
	start := time.Now()
	lastStatus := 0
	for _, url := range endpoints {
		status, n, err := try(url)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			return TestResult{OK: false, Detail: "Could not reach the provider: " + err.Error(), LatencyMS: latency}
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return TestResult{OK: false, Detail: fmt.Sprintf("Provider rejected the credential (HTTP %d).", status), LatencyMS: latency}
		}
		if status == http.StatusOK && n > 0 {
			return TestResult{OK: true, Detail: fmt.Sprintf("Valid — %d model(s) available.", n), LatencyMS: latency}
		}
		lastStatus = status
	}
	return TestResult{
		OK:        false,
		Detail:    fmt.Sprintf("Reachable (HTTP %d) but no OpenAI-compatible model list found — check the base URL includes the API path (e.g. http://localhost:1234/v1).", lastStatus),
		LatencyMS: time.Since(start).Milliseconds(),
	}
}

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
	if bu := strings.TrimSpace(fields["base_url"]); bu != "" {
		auth := ""
		if fields["api_key"] != "" {
			auth = "Authorization: Bearer {api_key}"
		}
		return &Verify{
			Endpoint:     strings.TrimRight(bu, "/") + "/models",
			AuthTemplate: auth,
		}, true
	}
	return nil, false
}

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
