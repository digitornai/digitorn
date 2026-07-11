package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/gitrepo"
	"github.com/go-chi/chi/v5"
)

const githubProviderKey = "github"

// githubWorkspaceAppID tags the workspace GitHub OAuth flow's pending state so the
// shared MCP callback can tell it apart from a connector-piece flow (@pieces) or a
// managed MCP server (@mcp-managed) and route it to the direct-provider exchange.
const githubWorkspaceAppID = "@github-workspace"

type githubLinkState struct {
	FullName string `json:"full_name"` // owner/repo
	URL      string `json:"url"`       // https://github.com/owner/repo
	Branch   string `json:"branch"`
	LinkedAt string `json:"linked_at"`
	LastPush string `json:"last_push,omitempty"`
}

func githubLinkPath(workdir string) string {
	return filepath.Join(workdir, ".digitorn", "github.json")
}

func readGithubLink(workdir string) *githubLinkState {
	b, err := os.ReadFile(githubLinkPath(workdir))
	if err != nil {
		return nil
	}
	var st githubLinkState
	if json.Unmarshal(b, &st) != nil || st.FullName == "" {
		return nil
	}
	return &st
}

func writeGithubLink(workdir string, st *githubLinkState) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.MkdirAll(filepath.Dir(githubLinkPath(workdir)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(githubLinkPath(workdir), b, 0o600)
}

func (d *Daemon) githubToken(r *http.Request) (string, error) {
	if d.mcpOAuth == nil {
		return "", errors.New("oauth is not configured on this daemon")
	}
	tok, err := d.mcpOAuth.GetToken(r.Context(), userIDOf(r.Context()), githubProviderKey)
	if err != nil || tok == nil || tok.AccessToken == "" {
		return "", errors.New("github not connected")
	}
	return tok.AccessToken, nil
}

// githubOAuthCreds resolves Digitorn's managed GitHub OAuth app credentials.
// Hub first (the /system channel every daemon — cloud or self-hosted — already
// uses for connectors), so no per-install secrets are needed; env vars are a
// local-dev fallback / override only.
func (d *Daemon) githubOAuthCreds(r *http.Request) (clientID, clientSecret string) {
	if d.mcpHub != nil {
		if sys, err := d.mcpHub.PiecesSystemConfig(r.Context(), githubProviderKey); err == nil && sys != nil {
			clientID, _ = sys.DigitornProvided["oauth_client_id"].(string)
			clientSecret, _ = sys.DigitornProvided["oauth_client_secret"].(string)
		}
	}
	if clientID == "" {
		clientID = os.Getenv("DIGITORN_GITHUB_CLIENT_ID")
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("DIGITORN_GITHUB_CLIENT_SECRET")
	}
	return clientID, clientSecret
}

// POST /api/github/oauth/start → {auth_url}
func (d *Daemon) githubOAuthStart(w http.ResponseWriter, r *http.Request) {
	if d.mcpOAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth_unavailable", "OAuth is not configured on this daemon.")
		return
	}
	// Digitorn's managed GitHub OAuth app lives in the hub — the SAME source the
	// github connector uses (/api/v1/pieces/github/system). A self-hosted daemon
	// thus needs no local secrets: it fetches them over the daemon-key channel.
	// The env vars are only a local-dev fallback / override.
	clientID, clientSecret := d.githubOAuthCreds(r)
	if clientID == "" || clientSecret == "" {
		writeError(w, http.StatusServiceUnavailable, "github_oauth_unconfigured",
			"GitHub OAuth app is not provisioned in the hub (and no local override set)")
		return
	}
	cfg := &schema.MCPAuthConfig{
		Type:     "oauth2",
		Provider: githubProviderKey,
		Scopes:   []string{"repo"},
	}
	authURL, state, err := d.mcpOAuth.AuthorizeForPiece(
		r.Context(), cfg, userIDOf(r.Context()), githubWorkspaceAppID, githubProviderKey, clientID, clientSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oauth_start_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth_url": authURL, "state": state})
}

// GET /api/apps/{app_id}/sessions/{session_id}/github/status
func (d *Daemon) githubStatus(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	connected := false
	if _, terr := d.githubToken(r); terr == nil {
		connected = true
	}
	out := map[string]any{"connected": connected}
	if st := readGithubLink(wd); st != nil {
		out["repo"] = st.FullName
		out["url"] = st.URL
		out["branch"] = st.Branch
		out["last_push"] = st.LastPush
	}
	writeJSON(w, http.StatusOK, out)
}

// POST .../github/link {repo, create, private} — create and/or bind the repo.
func (d *Daemon) githubLink(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	token, err := d.githubToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "github_not_connected", err.Error())
		return
	}
	var req struct {
		Repo    string `json:"repo"`
		Create  bool   `json:"create"`
		Private bool   `json:"private"`
	}
	if err := readJSONLenient(r, &req); err != nil || strings.TrimSpace(req.Repo) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "repo is required")
		return
	}
	full := strings.TrimSpace(req.Repo)
	if req.Create {
		full, err = githubCreateRepo(r.Context().Done(), token, full, req.Private)
		if err != nil {
			writeError(w, http.StatusBadGateway, "github_create_failed", err.Error())
			return
		}
	} else if !strings.Contains(full, "/") {
		login, lerr := githubLogin(token)
		if lerr != nil {
			writeError(w, http.StatusBadGateway, "github_user_failed", lerr.Error())
			return
		}
		full = login + "/" + full
	}
	st := &githubLinkState{
		FullName: full,
		URL:      "https://github.com/" + full,
		Branch:   "main",
		LinkedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeGithubLink(wd, st); err != nil {
		writeError(w, http.StatusInternalServerError, "link_persist_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": st.FullName, "url": st.URL, "branch": st.Branch})
}

// POST .../github/push {message} — commit pending work and push HEAD.
func (d *Daemon) githubPush(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	token, err := d.githubToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "github_not_connected", err.Error())
		return
	}
	st := readGithubLink(wd)
	if st == nil {
		writeError(w, http.StatusConflict, "github_not_linked", "link a repository first")
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	_ = readJSONLenient(r, &req)
	repo, err := gitrepo.Open(wd)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "git_open_failed", err.Error())
		return
	}
	if err := repo.StageAll(); err != nil {
		writeError(w, http.StatusInternalServerError, "git_stage_failed", err.Error())
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		msg = "digitorn: workspace push"
	}
	commit, cerr := repo.Commit(msg, nil)
	if cerr != nil && !errors.Is(cerr, gitrepo.ErrNothingStaged) {
		writeError(w, http.StatusInternalServerError, "git_commit_failed", cerr.Error())
		return
	}
	remote := "https://github.com/" + st.FullName + ".git"
	if err := repo.PushToRemote(remote, token, st.Branch); err != nil {
		writeError(w, http.StatusBadGateway, "git_push_failed", err.Error())
		return
	}
	st.LastPush = time.Now().UTC().Format(time.RFC3339)
	_ = writeGithubLink(wd, st)
	writeJSON(w, http.StatusOK, map[string]any{
		"repo": st.FullName, "url": st.URL, "branch": st.Branch, "commit": commit,
	})
}

func githubAPI(token, method, path string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	cl := &http.Client{Timeout: 20 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}

func githubLogin(token string) (string, error) {
	b, code, err := githubAPI(token, "GET", "/user", nil)
	if err != nil || code != 200 {
		return "", fmt.Errorf("github /user: %d %v", code, err)
	}
	var u struct {
		Login string `json:"login"`
	}
	if json.Unmarshal(b, &u) != nil || u.Login == "" {
		return "", errors.New("github /user: bad response")
	}
	return u.Login, nil
}

func githubCreateRepo(_ <-chan struct{}, token, name string, private bool) (string, error) {
	short := name
	if i := strings.LastIndexByte(short, '/'); i >= 0 {
		short = short[i+1:]
	}
	b, code, err := githubAPI(token, "POST", "/user/repos", map[string]any{
		"name": short, "private": private, "auto_init": false,
	})
	if err != nil {
		return "", err
	}
	if code != 201 && code != 200 {
		return "", fmt.Errorf("github create repo: %d %s", code, strings.TrimSpace(string(b)))
	}
	var out struct {
		FullName string `json:"full_name"`
	}
	if json.Unmarshal(b, &out) != nil || out.FullName == "" {
		return "", errors.New("github create repo: bad response")
	}
	return out.FullName, nil
}
