package credentials

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/persistence/models"
)

// GitHub Copilot device-flow auth. The user authorizes a device code on
// github.com; we poll for the resulting GitHub OAuth token and store it in the
// vault as a `github_copilot` credential (the token lives in the `api_key`
// field). Listing models exchanges that token for a short-lived Copilot session
// token and hits api.githubcopilot.com — mirrors the editor flow.
const (
	copilotProvider   = "github_copilot"
	copilotClientID   = "Iv1.b507a08c87ecfe98"
	ghDeviceCodeURL   = "https://github.com/login/device/code"
	ghTokenURL        = "https://github.com/login/oauth/access_token"
	ghCopilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	copilotAPIBase    = "https://api.githubcopilot.com"
)

func copilotEditorHeaders(req *http.Request) {
	req.Header.Set("Editor-Version", "vscode/1.96.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.27.0")
	req.Header.Set("User-Agent", "GithubCopilot/1.270.0")
	req.Header.Set("Accept", "application/json")
}

// DeviceFlow is one in-progress device authorization (in memory, ephemeral).
type DeviceFlow struct {
	State           string
	deviceCode      string
	UserCode        string
	VerificationURI string
	Interval        int // seconds GitHub wants between polls
	ExpiresAt       time.Time
	Status          string // pending | connected | error | expired
	CredentialID    string
	Err             string

	mu       sync.Mutex // serializes polls of THIS flow
	lastPoll time.Time  // last time we actually hit GitHub (throttle)
}

// Public is the client-facing shape — never leaks the device_code.
func (f *DeviceFlow) Public() map[string]any {
	var credID, errStr any
	if f.CredentialID != "" {
		credID = f.CredentialID
	}
	if f.Err != "" {
		errStr = f.Err
	}
	expiresIn := int(time.Until(f.ExpiresAt).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}
	return map[string]any{
		"state":            f.State,
		"user_code":        f.UserCode,
		"verification_uri": f.VerificationURI,
		"expires_in":       expiresIn,
		"interval":         f.Interval,
		"status":           f.Status,
		"credential_id":    credID,
		"error":            errStr,
	}
}

type copilotFlows struct {
	mu    sync.Mutex
	flows map[string]*DeviceFlow
	httpc *http.Client
}

func newCopilotFlows() *copilotFlows {
	return &copilotFlows{
		flows: map[string]*DeviceFlow{},
		httpc: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *copilotFlows) put(f *DeviceFlow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Opportunistic sweep of stale flows.
	for s, fl := range c.flows {
		if time.Since(fl.ExpiresAt) > time.Hour {
			delete(c.flows, s)
		}
	}
	c.flows[f.State] = f
}

func (c *copilotFlows) get(state string) *DeviceFlow {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flows[state]
}

func newState() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CopilotStart kicks off a device flow and returns the user code + verification URL.
func (s *Store) CopilotStart(ctx context.Context) (*DeviceFlow, error) {
	body, _ := json.Marshal(map[string]string{"client_id": copilotClientID, "scope": "read:user"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ghDeviceCodeURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	copilotEditorHeaders(req)
	resp, err := s.copilot.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach github device endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github device-flow refused (HTTP %d)", resp.StatusCode)
	}
	var d struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Interval        int    `json:"interval"`
		ExpiresIn       int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || d.DeviceCode == "" || d.UserCode == "" {
		return nil, fmt.Errorf("github device-flow returned no codes")
	}
	if d.VerificationURI == "" {
		d.VerificationURI = "https://github.com/login/device"
	}
	if d.Interval == 0 {
		d.Interval = 5
	}
	if d.ExpiresIn == 0 {
		d.ExpiresIn = 900
	}
	flow := &DeviceFlow{
		State:           newState(),
		deviceCode:      d.DeviceCode,
		UserCode:        d.UserCode,
		VerificationURI: d.VerificationURI,
		Interval:        d.Interval,
		ExpiresAt:       time.Now().Add(time.Duration(d.ExpiresIn) * time.Second),
		Status:          "pending",
	}
	s.copilot.put(flow)
	return flow, nil
}

// CopilotPoll hits GitHub once for the given flow; on success it persists the
// GitHub token as a per-user `github_copilot` credential.
func (s *Store) CopilotPoll(ctx context.Context, userID, state string) (*DeviceFlow, error) {
	flow := s.copilot.get(state)
	if flow == nil {
		return nil, ErrNotFound
	}
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.Status != "pending" {
		return flow, nil
	}
	if time.Now().After(flow.ExpiresAt) {
		flow.Status = "expired"
		flow.Err = "Device code expired before authorization."
		return flow, nil
	}
	// Throttle: GitHub rate-limits the device flow and ESCALATES the required
	// interval on every premature poll (a runaway slow_down). The web polls
	// every few seconds; we absorb that and only forward to GitHub once its
	// interval has elapsed, so it never escalates and the flow completes.
	if !flow.lastPoll.IsZero() && time.Since(flow.lastPoll) < time.Duration(flow.Interval)*time.Second {
		return flow, nil
	}
	flow.lastPoll = time.Now()

	body, _ := json.Marshal(map[string]string{
		"client_id":   copilotClientID,
		"device_code": flow.deviceCode,
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ghTokenURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	copilotEditorHeaders(req)
	resp, err := s.copilot.httpc.Do(req)
	if err != nil {
		return flow, nil // transient: stay pending, client retries
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var d struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Interval         int    `json:"interval"`
	}
	_ = json.Unmarshal(raw, &d)
	// Respect GitHub's requested interval so we don't trip the rate limiter.
	if d.Interval > flow.Interval {
		flow.Interval = d.Interval
	}

	if d.AccessToken != "" {
		view, cerr := s.Create(ctx, userID, CreateInput{
			ProviderName: copilotProvider,
			ProviderType: "device_code",
			Label:        "GitHub Copilot",
			Name:         copilotProvider,
			Fields:       map[string]string{"api_key": d.AccessToken},
		})
		if cerr != nil {
			flow.Status = "error"
			flow.Err = "Authorized, but saving the credential failed: " + cerr.Error()
			return flow, nil
		}
		flow.Status = "connected"
		flow.CredentialID = view.ID
		return flow, nil
	}

	switch d.Error {
	case "authorization_pending":
		// user hasn't entered the code yet
	case "slow_down":
		if flow.Interval < 10 {
			flow.Interval = 10
		}
	case "expired_token", "expired_token_request":
		flow.Status = "expired"
		flow.Err = "Device code expired before authorization."
	case "access_denied", "incorrect_device_code", "incorrect_client_credentials":
		flow.Status = "error"
		flow.Err = strings.TrimSpace(d.Error + ": " + d.ErrorDescription)
	}
	return flow, nil
}

// CopilotModel is one model a Copilot subscription exposes.
type CopilotModel struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Vendor            string `json:"vendor"`
	Version           string `json:"version"`
	Preview           bool   `json:"preview"`
	ContextWindow     *int   `json:"context_window"`
	MaxOutputTokens   *int   `json:"max_output_tokens"`
	SupportsToolCalls bool   `json:"supports_tool_calls"`
	SupportsVision    bool   `json:"supports_vision"`
}

// CopilotModels lists the chat models the stored Copilot credential allows.
func (s *Store) CopilotModels(ctx context.Context, userID, credentialID string) (string, []CopilotModel, error) {
	id, fields, err := s.revealForProvider(ctx, userID, copilotProvider, credentialID)
	if err != nil {
		return "", nil, err
	}
	ghToken := fields["api_key"]
	if ghToken == "" {
		return "", nil, fmt.Errorf("credential has no api_key field")
	}
	copilotToken, err := s.exchangeCopilotToken(ctx, ghToken)
	if err != nil {
		return "", nil, err
	}
	models, err := s.fetchCopilotModels(ctx, copilotToken)
	if err != nil {
		return "", nil, err
	}
	return id, models, nil
}

func (s *Store) exchangeCopilotToken(ctx context.Context, ghToken string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ghCopilotTokenURL, nil)
	req.Header.Set("Authorization", "token "+ghToken)
	copilotEditorHeaders(req)
	resp, err := s.copilot.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach api.github.com: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "", fmt.Errorf("github rejected the token (HTTP %d) — it may be revoked or lack Copilot access", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("copilot token exchange failed (HTTP %d)", resp.StatusCode)
	}
	var d struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || d.Token == "" {
		return "", fmt.Errorf("copilot token endpoint returned no token")
	}
	return d.Token, nil
}

func (s *Store) fetchCopilotModels(ctx context.Context, copilotToken string) ([]CopilotModel, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, copilotAPIBase+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+copilotToken)
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
	copilotEditorHeaders(req)
	resp, err := s.copilot.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach api.githubcopilot.com: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("copilot models endpoint failed (HTTP %d)", resp.StatusCode)
	}
	var d struct {
		Data []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			Vendor       string `json:"vendor"`
			Version      string `json:"version"`
			Preview      bool   `json:"preview"`
			Capabilities struct {
				Type   string `json:"type"`
				Limits struct {
					MaxContextWindowTokens *int `json:"max_context_window_tokens"`
					MaxOutputTokens        *int `json:"max_output_tokens"`
				} `json:"limits"`
				Supports struct {
					ToolCalls bool `json:"tool_calls"`
					Vision    bool `json:"vision"`
				} `json:"supports"`
			} `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("copilot models response unparseable")
	}
	out := make([]CopilotModel, 0, len(d.Data))
	for _, m := range d.Data {
		if m.ID == "" {
			continue
		}
		if t := m.Capabilities.Type; t != "" && t != "chat" {
			continue // surface chat models only
		}
		out = append(out, CopilotModel{
			ID:                m.ID,
			Name:              orStr(m.Name, m.ID),
			Vendor:            m.Vendor,
			Version:           m.Version,
			Preview:           m.Preview,
			ContextWindow:     m.Capabilities.Limits.MaxContextWindowTokens,
			MaxOutputTokens:   m.Capabilities.Limits.MaxOutputTokens,
			SupportsToolCalls: m.Capabilities.Supports.ToolCalls,
			SupportsVision:    m.Capabilities.Supports.Vision,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Vendor != out[j].Vendor {
			return out[i].Vendor < out[j].Vendor
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// revealForProvider finds the user's credential for a provider (optionally a
// specific id) and returns its id + decrypted fields. INTERNAL only.
func (s *Store) revealForProvider(ctx context.Context, userID, provider, credentialID string) (string, map[string]string, error) {
	q := s.db.WithContext(ctx).Where("user_id = ? AND provider_name = ?", userID, provider)
	if credentialID != "" {
		q = q.Where("id = ?", credentialID)
	}
	var row models.UserCredential
	if err := q.Order("updated_at DESC").First(&row).Error; err != nil {
		return "", nil, ErrNotFound
	}
	plain, err := s.sealer.Open(row.Sealed)
	if err != nil {
		return "", nil, err
	}
	fields := map[string]string{}
	if len(plain) > 0 {
		_ = json.Unmarshal(plain, &fields)
	}
	return row.ID, fields, nil
}

func orStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
