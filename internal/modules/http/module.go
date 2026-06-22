package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	gohttp "net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/flexjson"
	"github.com/mbathepaul/digitorn/internal/modules/eventemitter"
	"github.com/mbathepaul/digitorn/internal/safehttp"
	"github.com/mbathepaul/digitorn/pkg/module"
)

const (
	defaultTimeout = 30 * time.Second
	maxBody        = 10 << 20
)

type Config struct {
	AllowPrivateHosts bool               `json:"allow_private_hosts" yaml:"allow_private_hosts"`
	AllowedHosts      []string           `json:"allowed_hosts"       yaml:"allowed_hosts"`
	BlockedHosts      []string           `json:"blocked_hosts"       yaml:"blocked_hosts"`
	TimeoutSecs       float64            `json:"timeout"             yaml:"timeout"`
	DefaultHeaders    map[string]string  `json:"default_headers"     yaml:"default_headers"`
	Credentials       map[string]credDef `json:"credentials"         yaml:"credentials"`
}

type credDef struct {
	Type     string `json:"type"     yaml:"type"`
	Token    string `json:"token"    yaml:"token"`
	Username string `json:"username" yaml:"username"`
	Password string `json:"password" yaml:"password"`
	Header   string `json:"header"   yaml:"header"`
}

type Module struct {
	module.Base
	cfg    Config
	client *gohttp.Client
}

func New() *Module {
	m := &Module{}
	m.Base = module.Base{
		ID:          "http",
		Version:     "1.0.0",
		Description: "Make HTTP requests to external APIs — SSRF-guarded, with auth and file upload/download.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux, domainmodule.PlatformMacOS, domainmodule.PlatformWindows,
		},
	}
	m.client = safehttp.Client(defaultTimeout, false, nil)

	baseParams := []tool.ParamSpec{
		{Name: "url", Type: "string", Description: "Target URL.", Required: true},
		{Name: "headers", Type: "object", Description: "Additional HTTP headers."},
		{Name: "body", Type: "string", Description: "Request body."},
		{Name: "auth", Type: "string", Description: "Credential name from config or inline 'Bearer <token>'."},
		{Name: "timeout", Type: "integer", Description: "Timeout in seconds (default 30)."},
	}
	shortParams := []tool.ParamSpec{baseParams[0], baseParams[1], baseParams[3], baseParams[4]}

	for _, entry := range []struct {
		name  string
		short bool
	}{
		{"get", true}, {"post", false}, {"put", false},
		{"patch", false}, {"delete", true}, {"head", true},
	} {
		meth := strings.ToUpper(entry.name)
		p := baseParams
		if entry.short {
			p = shortParams
		}
		m.RegisterTool(module.Tool{
			Name:        entry.name,
			Description: "HTTP " + meth + " request.",
			Params:      p,
			RiskLevel:   tool.RiskLow,
			Tags:        []string{"http", entry.name},
			CLILabel:    meth,
			CLIParam:    "url",
			Handler: func(meth string) func(context.Context, json.RawMessage) (tool.Result, error) {
				return func(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
					return m.doRequest(ctx, meth, raw)
				}
			}(meth),
		})
	}

	m.RegisterTool(module.Tool{
		Name:        "request",
		Description: "Generic HTTP request with explicit method.",
		Params: append([]tool.ParamSpec{
			{Name: "method", Type: "string", Description: "HTTP method.", Required: true},
		}, baseParams...),
		RiskLevel: tool.RiskLow,
		Tags:      []string{"http"},
		CLILabel:  "Request", CLIParam: "url",
		Handler: m.request,
	})

	m.RegisterTool(module.Tool{
		Name:        "download",
		Description: "Download a URL to a local file.",
		Params: []tool.ParamSpec{
			{Name: "url", Type: "string", Required: true},
			{Name: "path", Type: "string", Description: "Local path to save to.", Required: true, Path: true},
			{Name: "auth", Type: "string"},
		},
		RiskLevel: tool.RiskMedium,
		Tags:      []string{"http", "file"},
		CLILabel:  "Download", CLIParam: "url",
		Handler: m.download,
	})

	m.RegisterTool(module.Tool{
		Name:        "upload",
		Description: "Upload a local file to a URL via multipart/form-data.",
		Params: []tool.ParamSpec{
			{Name: "url", Type: "string", Required: true},
			{Name: "path", Type: "string", Description: "File to upload.", Required: true, Path: true},
			{Name: "field", Type: "string", Description: "Form field name (default 'file')."},
			{Name: "headers", Type: "object"},
			{Name: "auth", Type: "string"},
		},
		RiskLevel: tool.RiskMedium,
		Tags:      []string{"http", "file"},
		CLILabel:  "Upload", CLIParam: "url",
		Handler: m.upload,
	})

	return m
}

// DeclaredEvents returns the list of event topics this module may emit.
// Implements domainmodule.EventEmitter.
func (m *Module) DeclaredEvents() []map[string]string {
	return []map[string]string{
		{"topic": "http.request.success", "type": "request.success"},
		{"topic": "http.request.error", "type": "request.error"},
		{"topic": "http.request.timeout", "type": "request.timeout"},
	}
}

func (m *Module) Init(_ context.Context, cfg map[string]any) error {
	var c Config
	if err := m.BindConfig(cfg, &c); err != nil {
		return err
	}
	m.cfg = c
	timeout := defaultTimeout
	if c.TimeoutSecs > 0 {
		timeout = time.Duration(c.TimeoutSecs * float64(time.Second))
	}
	m.client = safehttp.Client(timeout, c.AllowPrivateHosts, nil)
	return nil
}

type reqParams struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    flexjson.Content  `json:"body"`
	Auth    string            `json:"auth"`
	Timeout flexjson.Int      `json:"timeout"`
}

func (m *Module) request(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p reqParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	if p.Method == "" {
		p.Method = "GET"
	}
	return m.doRequest(ctx, strings.ToUpper(p.Method), raw)
}

func (m *Module) doRequest(ctx context.Context, method string, raw json.RawMessage) (tool.Result, error) {
	var p reqParams
	_ = json.Unmarshal(raw, &p)

	if err := m.checkHost(p.URL); err != nil {
		return errResult(err), err
	}

	body := string(p.Body)
	var bodyR io.Reader
	if body != "" {
		bodyR = strings.NewReader(body)
	}

	client := m.client
	if p.Timeout > 0 {
		client = safehttp.Client(time.Duration(int(p.Timeout))*time.Second, m.cfg.AllowPrivateHosts, nil)
	}

	req, err := gohttp.NewRequestWithContext(ctx, method, p.URL, bodyR)
	if err != nil {
		return errResult(err), err
	}
	for k, v := range m.cfg.DefaultHeaders {
		req.Header.Set(k, v)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	m.applyAuth(req, p.Auth)
	if body != "" && req.Header.Get("Content-Type") == "" {
		if json.Valid([]byte(body)) {
			req.Header.Set("Content-Type", "application/json")
		} else {
			req.Header.Set("Content-Type", "text/plain")
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		// Check if it's a timeout error
		if ctx.Err() == context.DeadlineExceeded {
			eventemitter.EmitWithModule(ctx, "http", "http.request.timeout", map[string]any{
				"url":     p.URL,
				"method":  method,
				"timeout": p.Timeout,
			})
		} else {
			eventemitter.EmitWithModule(ctx, "http", "http.request.error", map[string]any{
				"url":    p.URL,
				"method": method,
				"error":  err.Error(),
			})
		}
		return errResult(err), err
	}
	defer resp.Body.Close()

	// Emit success event
	eventemitter.EmitWithModule(ctx, "http", "http.request.success", map[string]any{
		"url":          p.URL,
		"method":       method,
		"status_code":  resp.StatusCode,
		"content_type": resp.Header.Get("Content-Type"),
	})

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	ct := resp.Header.Get("Content-Type")

	data := map[string]any{
		"status":       resp.StatusCode,
		"ok":           resp.StatusCode >= 200 && resp.StatusCode < 300,
		"body":         string(respBody),
		"content_type": ct,
		"headers":      flatHeaders(resp.Header),
		"url":          resp.Request.URL.String(),
	}
	if strings.Contains(ct, "application/json") {
		var j any
		if json.Unmarshal(respBody, &j) == nil {
			data["json"] = j
		}
	}
	return tool.Result{Success: true, Data: data}, nil
}

func (m *Module) download(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		URL  string `json:"url"`
		Path string `json:"path"`
		Auth string `json:"auth"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	if err := m.checkHost(p.URL); err != nil {
		return errResult(err), err
	}
	req, err := gohttp.NewRequestWithContext(ctx, gohttp.MethodGet, p.URL, nil)
	if err != nil {
		return errResult(err), err
	}
	m.applyAuth(req, p.Auth)
	resp, err := m.client.Do(req)
	if err != nil {
		return errResult(err), err
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(filepath.Dir(p.Path), 0755); err != nil {
		return errResult(err), err
	}
	f, err := os.Create(p.Path)
	if err != nil {
		return errResult(err), err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return errResult(err), err
	}
	return tool.Result{Success: true, Data: map[string]any{
		"path": p.Path, "bytes": n, "status": resp.StatusCode,
	}}, nil
}

func (m *Module) upload(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		URL     string            `json:"url"`
		Path    string            `json:"path"`
		Field   string            `json:"field"`
		Headers map[string]string `json:"headers"`
		Auth    string            `json:"auth"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	if err := m.checkHost(p.URL); err != nil {
		return errResult(err), err
	}
	if p.Field == "" {
		p.Field = "file"
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return errResult(err), err
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile(p.Field, filepath.Base(p.Path))
	io.Copy(fw, f)
	w.Close()

	req, err := gohttp.NewRequestWithContext(ctx, gohttp.MethodPost, p.URL, &buf)
	if err != nil {
		return errResult(err), err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	m.applyAuth(req, p.Auth)
	resp, err := m.client.Do(req)
	if err != nil {
		return errResult(err), err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return tool.Result{Success: true, Data: map[string]any{
		"status": resp.StatusCode,
		"ok":     resp.StatusCode >= 200 && resp.StatusCode < 300,
		"body":   string(body),
	}}, nil
}

func (m *Module) applyAuth(req *gohttp.Request, auth string) {
	if auth == "" {
		return
	}
	if cred, ok := m.cfg.Credentials[auth]; ok {
		switch cred.Type {
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+cred.Token)
		case "basic":
			req.SetBasicAuth(cred.Username, cred.Password)
		case "api_key":
			h := cred.Header
			if h == "" {
				h = "X-API-Key"
			}
			req.Header.Set(h, cred.Token)
		}
		return
	}
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		req.Header.Set("Authorization", auth)
	}
}

func (m *Module) checkHost(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	host := u.Hostname()
	for _, b := range m.cfg.BlockedHosts {
		if strings.EqualFold(host, b) {
			return fmt.Errorf("host %q is blocked", host)
		}
	}
	if len(m.cfg.AllowedHosts) > 0 {
		for _, a := range m.cfg.AllowedHosts {
			if strings.EqualFold(host, a) {
				return nil
			}
		}
		return fmt.Errorf("host %q is not in the allowed list", host)
	}
	return nil
}

func flatHeaders(h gohttp.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

func errResult(err error) tool.Result {
	return tool.Result{Success: false, Error: err.Error()}
}
