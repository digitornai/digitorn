package server

import (
	"bytes"
	"context"
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

const githubModeNative = "native"

type githubLinkState struct {
	FullName    string `json:"full_name"` // owner/repo
	URL         string `json:"url"`       // https://github.com/owner/repo
	Branch      string `json:"branch"`
	Mode        string `json:"mode,omitempty"`
	LinkedAt    string `json:"linked_at"`
	LastPush    string `json:"last_push,omitempty"`
	LastPushSHA string `json:"last_push_sha,omitempty"` // HEAD at the last successful push — drives the ahead count
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
	uid := userIDOf(r.Context())
	if tok, err := d.mcpOAuth.GetToken(r.Context(), uid, githubProviderKey); err == nil && tok != nil && tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	if pm := d.piecesModule(); pm != nil {
		if store := pm.PiecesStore(); store != nil {
			if wire, werr := store.RevealAuth(r.Context(), uid, githubProviderKey); werr == nil && wire != nil && wire.AccessToken != "" {
				return wire.AccessToken, nil
			}
		}
	}
	return "", errors.New("github not connected")
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

// githubRepoView is one repo in the picker list (a trimmed GitHub repo).
type githubRepoView struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	PushedAt      string `json:"pushed_at"`
	URL           string `json:"url"`
}

// GET /api/github/repos — the connected user's repositories, most-recently-pushed
// first, for the "open a repo" picker. User-scoped (no session needed): the
// empty-state calls it before any workspace exists.
func (d *Daemon) githubRepos(w http.ResponseWriter, r *http.Request) {
	token, err := d.githubToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "github_not_connected", err.Error())
		return
	}
	b, code, err := githubAPI(token, "GET",
		"/user/repos?per_page=100&sort=pushed&affiliation=owner,collaborator,organization_member", nil)
	if err != nil || code != 200 {
		writeError(w, http.StatusBadGateway, "github_repos_failed",
			fmt.Sprintf("github /user/repos: %d %v", code, err))
		return
	}
	var raw []struct {
		FullName      string `json:"full_name"`
		Name          string `json:"name"`
		Private       bool   `json:"private"`
		Description   string `json:"description"`
		DefaultBranch string `json:"default_branch"`
		PushedAt      string `json:"pushed_at"`
		HTMLURL       string `json:"html_url"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		writeError(w, http.StatusBadGateway, "github_repos_parse", err.Error())
		return
	}
	repos := make([]githubRepoView, 0, len(raw))
	for _, x := range raw {
		repos = append(repos, githubRepoView{
			FullName: x.FullName, Name: x.Name, Private: x.Private,
			Description: x.Description, DefaultBranch: x.DefaultBranch,
			PushedAt: x.PushedAt, URL: x.HTMLURL,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos, "count": len(repos)})
}

// POST .../github/clone {repo, branch} — clone a GitHub repo into an EMPTY
// session workspace (native mode: push/pull operate on the real .git).
func longGitOp(w http.ResponseWriter, r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	return context.WithTimeout(context.WithoutCancel(r.Context()), timeout)
}

func (d *Daemon) githubClone(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	token, err := d.githubToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "github_not_connected", err.Error())
		return
	}
	var req struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
	}
	if err := readJSONLenient(r, &req); err != nil || strings.TrimSpace(req.Repo) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "repo is required")
		return
	}
	full := strings.TrimSpace(req.Repo)
	if !strings.Contains(full, "/") {
		login, lerr := githubLogin(token)
		if lerr != nil {
			writeError(w, http.StatusBadGateway, "github_user_failed", lerr.Error())
			return
		}
		full = login + "/" + full
	}
	ctx, cancel := longGitOp(w, r, 30*time.Minute)
	defer cancel()
	branch, head, err := gitrepo.CloneRepo(ctx, wd, "https://github.com/"+full+".git", strings.TrimSpace(req.Branch), token)
	if err != nil {
		if errors.Is(err, gitrepo.ErrWorkdirNotEmpty) {
			writeError(w, http.StatusConflict, "workspace_not_empty", err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "github_clone_failed", err.Error())
		return
	}
	st := &githubLinkState{
		FullName:    full,
		URL:         "https://github.com/" + full,
		Branch:      branch,
		Mode:        githubModeNative,
		LinkedAt:    time.Now().UTC().Format(time.RFC3339),
		LastPushSHA: head,
	}
	if err := writeGithubLink(wd, st); err != nil {
		writeError(w, http.StatusInternalServerError, "link_persist_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repo": st.FullName, "url": st.URL, "branch": st.Branch,
		"head": head, "mode": githubModeNative,
	})
}

// POST .../github/create {repo, private} — create a NEW GitHub repo and bind an
// empty session workspace to it (native mode from birth: init + origin remote;
// the first sync pushes everything).
func (d *Daemon) githubCreate(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	if wd == "" {
		writeError(w, http.StatusBadRequest, "no_workdir", "session has no workdir")
		return
	}
	token, err := d.githubToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "github_not_connected", err.Error())
		return
	}
	if cur := readGithubLink(wd); cur != nil && cur.Mode == githubModeNative {
		writeError(w, http.StatusConflict, "github_cloned_workspace", "this workspace is already bound to a repository")
		return
	}
	var req struct {
		Repo    string `json:"repo"`
		Private bool   `json:"private"`
	}
	if err := readJSONLenient(r, &req); err != nil || strings.TrimSpace(req.Repo) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "repo is required")
		return
	}
	full, err := githubCreateRepo(r.Context().Done(), token, strings.TrimSpace(req.Repo), req.Private)
	if err != nil {
		writeError(w, http.StatusBadGateway, "github_create_failed", err.Error())
		return
	}
	if err := gitrepo.InitRepo(wd, "https://github.com/"+full+".git", "main"); err != nil {
		if errors.Is(err, gitrepo.ErrWorkdirNotEmpty) {
			writeError(w, http.StatusConflict, "workspace_not_empty", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "git_init_failed", err.Error())
		return
	}
	st := &githubLinkState{
		FullName: full,
		URL:      "https://github.com/" + full,
		Branch:   "main",
		Mode:     githubModeNative,
		LinkedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeGithubLink(wd, st); err != nil {
		writeError(w, http.StatusInternalServerError, "link_persist_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repo": st.FullName, "url": st.URL, "branch": st.Branch, "mode": githubModeNative,
	})
}

// POST .../github/pull — fast-forward a cloned workspace from origin.
func (d *Daemon) githubPull(w http.ResponseWriter, r *http.Request) {
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
	if st == nil || st.Mode != githubModeNative {
		writeError(w, http.StatusConflict, "github_not_cloned", "this workspace is not a cloned repository")
		return
	}
	ctx, cancel := longGitOp(w, r, 30*time.Minute)
	defer cancel()
	head, updated, err := gitrepo.NativePull(ctx, wd, token)
	if err != nil {
		writeError(w, http.StatusConflict, "git_pull_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repo": st.FullName, "branch": st.Branch, "head": head, "updated": updated,
	})
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
	out := map[string]any{"connected": connected, "empty": !gitrepo.WorkdirHasContent(wd)}
	if st := readGithubLink(wd); st != nil {
		out["repo"] = st.FullName
		out["url"] = st.URL
		out["branch"] = st.Branch
		out["last_push"] = st.LastPush
		if st.Mode != "" {
			out["mode"] = st.Mode
		}
		if st.Mode == githubModeNative {
			if unc, head, nerr := gitrepo.NativeStatus(wd); nerr == nil {
				out["uncommitted"] = unc
				out["head"] = head
			}
		} else if repo, oerr := gitrepo.Open(wd); oerr == nil {
			if changes, cerr := repo.Changes(); cerr == nil {
				out["uncommitted"] = len(changes)
			}
			if log, lerr := repo.Log(); lerr == nil {
				ahead := 0
				for _, c := range log {
					if c.Sha == st.LastPushSHA {
						break
					}
					ahead++
				}
				out["ahead"] = ahead
			}
		}
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
	if cur := readGithubLink(wd); cur != nil && cur.Mode == githubModeNative {
		writeError(w, http.StatusConflict, "github_cloned_workspace",
			"this workspace is a cloned repository; it cannot be re-linked")
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

// POST .../github/push — push the workspace's COMMITTED history to GitHub.
//
// The workspace Changes panel is the single commit authority: this handler never
// fabricates a commit. It pushes HEAD as-is (the commits the user/agent made in
// the panel) and reports any still-uncommitted changes so the UI can nudge the
// user to commit them first — non-blocking. It only refuses when nothing has been
// committed at all (there is no HEAD to push).
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
	if st.Mode == githubModeNative {
		var req struct {
			Message string `json:"message"`
		}
		_ = readJSONLenient(r, &req)
		msg := strings.TrimSpace(req.Message)
		if msg == "" {
			msg = "digitorn: workspace sync"
		}
		name, email := "", ""
		if login, lerr := githubLogin(token); lerr == nil {
			name, email = login, login+"@users.noreply.github.com"
		}
		ctx, cancel := longGitOp(w, r, 30*time.Minute)
		defer cancel()
		head, committed, serr := gitrepo.NativeSync(ctx, wd, token, msg, name, email, st.Branch)
		if serr != nil {
			writeError(w, http.StatusBadGateway, "git_push_failed", serr.Error())
			return
		}
		st.LastPush = time.Now().UTC().Format(time.RFC3339)
		st.LastPushSHA = head
		_ = writeGithubLink(wd, st)
		writeJSON(w, http.StatusOK, map[string]any{
			"repo": st.FullName, "url": st.URL, "branch": st.Branch,
			"commit": head, "committed": committed, "mode": githubModeNative,
		})
		return
	}
	repo, err := gitrepo.Open(wd)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "git_open_failed", err.Error())
		return
	}
	head, err := repo.HeadSHA()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "git_head_failed", err.Error())
		return
	}
	if head == "" {
		writeError(w, http.StatusConflict, "github_nothing_to_push",
			"commit your changes in the workspace first, then push")
		return
	}
	uncommitted := 0
	if changes, cerr := repo.Changes(); cerr == nil {
		uncommitted = len(changes)
	}
	remote := "https://github.com/" + st.FullName + ".git"
	if err := repo.PushToRemote(remote, token, st.Branch); err != nil {
		writeError(w, http.StatusBadGateway, "git_push_failed", err.Error())
		return
	}
	st.LastPush = time.Now().UTC().Format(time.RFC3339)
	st.LastPushSHA = head
	_ = writeGithubLink(wd, st)
	writeJSON(w, http.StatusOK, map[string]any{
		"repo": st.FullName, "url": st.URL, "branch": st.Branch,
		"commit": head, "uncommitted": uncommitted,
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
