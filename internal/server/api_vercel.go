package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/gitrepo"
	"github.com/digitornai/digitorn/internal/server/mcpoauth"
	"github.com/go-chi/chi/v5"
)

const vercelProviderKey = "vercel"

const vercelGithubAppInstallURL = "https://github.com/apps/vercel/installations/new"

var errVercelGithubAppMissing = errors.New("vercel: github app not installed on the repository")

var vercelAPIBase = "https://api.vercel.com"

type vercelState struct {
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	URL         string `json:"url"`
	TeamID      string `json:"team_id,omitempty"`
	DeployedAt  string `json:"deployed_at,omitempty"`
}

func vercelStatePath(workdir string) string {
	return filepath.Join(workdir, ".digitorn", "vercel.json")
}

func readVercelState(workdir string) *vercelState {
	b, err := os.ReadFile(vercelStatePath(workdir))
	if err != nil {
		return nil
	}
	var st vercelState
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

func writeVercelState(workdir string, st *vercelState) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.MkdirAll(filepath.Dir(vercelStatePath(workdir)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(vercelStatePath(workdir), b, 0o600)
}

func (d *Daemon) vercelAuth(r *http.Request) (token, teamID string, err error) {
	if d.mcpOAuth == nil {
		return "", "", errors.New("vercel not connected")
	}
	tok, terr := d.mcpOAuth.GetToken(r.Context(), userIDOf(r.Context()), vercelProviderKey)
	if terr != nil || tok == nil || strings.TrimSpace(tok.AccessToken) == "" {
		return "", "", errors.New("vercel not connected")
	}
	return tok.AccessToken, tok.Scope, nil
}

func (d *Daemon) vercelConnect(w http.ResponseWriter, r *http.Request) {
	if d.mcpOAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "credential_store_unavailable", "credential store is not configured")
		return
	}
	var req struct {
		Token  string `json:"token"`
		TeamID string `json:"team_id"`
	}
	if err := readJSONLenient(r, &req); err != nil || strings.TrimSpace(req.Token) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "token is required")
		return
	}
	tok := &mcpoauth.Token{AccessToken: strings.TrimSpace(req.Token), Scope: strings.TrimSpace(req.TeamID)}
	if err := d.mcpOAuth.SetManual(r.Context(), userIDOf(r.Context()), vercelProviderKey, tok); err != nil {
		writeError(w, http.StatusInternalServerError, "connect_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connected": true})
}

var vercelNameRe = regexp.MustCompile(`[^a-z0-9._]+`)

func vercelProjectName(fullName string) string {
	n := strings.ToLower(fullName)
	n = vercelNameRe.ReplaceAllString(n, "-")
	n = strings.Trim(n, "-._")
	if len(n) > 100 {
		n = n[:100]
	}
	if n == "" {
		n = "app"
	}
	return n
}

type vercelAPIError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type vercelProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Link struct {
		Org    string `json:"org"`
		Repo   string `json:"repo"`
		RepoID int    `json:"repoId"`
	} `json:"link"`
	Alias []struct {
		Domain string `json:"domain"`
	} `json:"alias"`
}

func vercelLiveURL(p *vercelProject, name string) string {
	for _, a := range p.Alias {
		if a.Domain != "" {
			return "https://" + a.Domain
		}
	}
	return "https://" + name + ".vercel.app"
}

func vercelRequest(ctx context.Context, method, path, token, teamID string, body any) ([]byte, int, error) {
	u := vercelAPIBase + path
	if teamID != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + "teamId=" + url.QueryEscape(teamID)
	}
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return data, resp.StatusCode, nil
}

func vercelParseErr(data []byte) *vercelAPIError {
	var e vercelAPIError
	if json.Unmarshal(data, &e) == nil && e.Error.Code != "" {
		return &e
	}
	return nil
}

func vercelIsGithubAppMissing(e *vercelAPIError) bool {
	if e == nil {
		return false
	}
	m := strings.ToLower(e.Error.Message)
	return strings.Contains(m, "github") && (strings.Contains(m, "install") || strings.Contains(m, "integration"))
}

func vercelCreateOrGetProject(ctx context.Context, token, teamID, name, repo string) (*vercelProject, bool, error) {
	body := map[string]any{
		"name": name,
		"gitRepository": map[string]any{
			"type": "github",
			"repo": repo,
		},
	}
	data, code, err := vercelRequest(ctx, http.MethodPost, "/v11/projects", token, teamID, body)
	if err != nil {
		return nil, false, err
	}
	if code >= 200 && code < 300 {
		var p vercelProject
		if json.Unmarshal(data, &p) != nil {
			return nil, false, errors.New("vercel: bad create-project response")
		}
		return &p, true, nil
	}
	e := vercelParseErr(data)
	if vercelIsGithubAppMissing(e) {
		return nil, false, errVercelGithubAppMissing
	}
	if e != nil && (e.Error.Code == "project_name_already_exists" || strings.Contains(strings.ToLower(e.Error.Message), "already exists")) {
		p, gerr := vercelGetProject(ctx, token, teamID, name)
		if gerr != nil {
			return nil, false, gerr
		}
		return p, false, nil
	}
	if e != nil {
		return nil, false, errors.New(e.Error.Message)
	}
	return nil, false, fmt.Errorf("vercel: create project HTTP %d", code)
}

func vercelGetProject(ctx context.Context, token, teamID, name string) (*vercelProject, error) {
	data, code, err := vercelRequest(ctx, http.MethodGet, "/v9/projects/"+url.PathEscape(name), token, teamID, nil)
	if err != nil {
		return nil, err
	}
	if code < 200 || code >= 300 {
		return nil, fmt.Errorf("vercel: get project HTTP %d", code)
	}
	var p vercelProject
	if json.Unmarshal(data, &p) != nil {
		return nil, errors.New("vercel: bad get-project response")
	}
	return &p, nil
}

func vercelTriggerDeployment(ctx context.Context, token, teamID, name string, p *vercelProject, branch string) (string, error) {
	if branch == "" {
		branch = "main"
	}
	git := map[string]any{"type": "github", "ref": branch}
	if p.Link.RepoID != 0 {
		git["repoId"] = p.Link.RepoID
	} else if p.Link.Org != "" && p.Link.Repo != "" {
		git["org"] = p.Link.Org
		git["repo"] = p.Link.Repo
	}
	body := map[string]any{
		"name":      name,
		"project":   p.ID,
		"target":    "production",
		"gitSource": git,
	}
	data, code, err := vercelRequest(ctx, http.MethodPost, "/v13/deployments?skipAutoDetectionConfirmation=1", token, teamID, body)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		if e := vercelParseErr(data); e != nil {
			return "", errors.New(e.Error.Message)
		}
		return "", fmt.Errorf("vercel: create deployment HTTP %d", code)
	}
	var dep struct {
		URL   string   `json:"url"`
		Alias []string `json:"alias"`
	}
	_ = json.Unmarshal(data, &dep)
	for _, a := range dep.Alias {
		if a != "" && !strings.Contains(a, "-git-") {
			return "https://" + a, nil
		}
	}
	if len(dep.Alias) > 0 && dep.Alias[0] != "" {
		return "https://" + dep.Alias[0], nil
	}
	if dep.URL != "" {
		return "https://" + dep.URL, nil
	}
	return "", nil
}

func (d *Daemon) vercelDeploy(w http.ResponseWriter, r *http.Request) {
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
	st := readGithubLink(wd)
	if st == nil || st.FullName == "" {
		writeError(w, http.StatusConflict, "no_repo", "connect and push a GitHub repo before deploying")
		return
	}
	token, teamID, err := d.vercelAuth(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "vercel_not_connected", "connect the Vercel connector first (paste a Vercel access token)")
		return
	}
	name := vercelProjectName(st.FullName)
	ctx, cancel := longGitOp(w, r, 15*time.Minute)
	defer cancel()
	if ghToken, gerr := d.githubToken(r); gerr == nil {
		author, email := "", ""
		if login, lerr := githubLogin(ghToken); lerr == nil {
			author, email = login, login+"@users.noreply.github.com"
		}
		if _, _, serr := gitrepo.NativeSync(ctx, wd, ghToken, "digitorn: deploy", author, email, st.Branch); serr != nil {
			writeError(w, http.StatusBadGateway, "git_push_failed", serr.Error())
			return
		}
	}
	proj, _, err := vercelCreateOrGetProject(ctx, token, teamID, name, st.FullName)
	if err != nil {
		if errors.Is(err, errVercelGithubAppMissing) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       "vercel_github_app_missing",
				"message":     "Install the Vercel GitHub App on your repository, then deploy again.",
				"install_url": vercelGithubAppInstallURL,
			})
			return
		}
		writeError(w, http.StatusBadGateway, "vercel_deploy_failed", err.Error())
		return
	}
	liveURL, derr := vercelTriggerDeployment(ctx, token, teamID, name, proj, st.Branch)
	if derr != nil {
		writeError(w, http.StatusBadGateway, "vercel_deploy_failed", derr.Error())
		return
	}
	if liveURL == "" {
		liveURL = vercelLiveURL(proj, name)
	}
	_ = writeVercelState(wd, &vercelState{
		ProjectID:   proj.ID,
		ProjectName: proj.Name,
		URL:         liveURL,
		TeamID:      teamID,
		DeployedAt:  time.Now().UTC().Format(time.RFC3339),
	})
	writeJSON(w, http.StatusOK, map[string]any{"url": liveURL, "project": proj.Name})
}

type vercelDeployment struct {
	ReadyState   string `json:"readyState"`
	State        string `json:"state"`
	CreatedAt    int64  `json:"createdAt"`
	Created      int64  `json:"created"`
	URL          string `json:"url"`
	InspectorURL string `json:"inspectorUrl"`
}

func vercelLatestDeployment(ctx context.Context, token, teamID, projectID string) *vercelDeployment {
	data, code, err := vercelRequest(ctx, http.MethodGet, "/v6/deployments?limit=1&projectId="+url.QueryEscape(projectID), token, teamID, nil)
	if err != nil || code < 200 || code >= 300 {
		return nil
	}
	var resp struct {
		Deployments []vercelDeployment `json:"deployments"`
	}
	if json.Unmarshal(data, &resp) != nil || len(resp.Deployments) == 0 {
		return nil
	}
	return &resp.Deployments[0]
}

func vercelDashboardURL(inspectorURL string) string {
	if inspectorURL == "" {
		return ""
	}
	i := strings.LastIndex(inspectorURL, "/")
	if i <= len("https:/") {
		return inspectorURL
	}
	return inspectorURL[:i]
}

func (d *Daemon) vercelStatus(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	connected := false
	if _, _, terr := d.vercelAuth(r); terr == nil {
		connected = true
	}
	out := map[string]any{"connected": connected}
	if wd != "" {
		if st := readVercelState(wd); st != nil {
			out["deployed"] = true
			out["url"] = st.URL
			out["project"] = st.ProjectName
			out["deployed_at"] = st.DeployedAt
			if connected && st.ProjectID != "" {
				token, teamID, _ := d.vercelAuth(r)
				ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
				if dep := vercelLatestDeployment(ctx, token, teamID, st.ProjectID); dep != nil {
					state := dep.ReadyState
					if state == "" {
						state = dep.State
					}
					out["state"] = state
					if ms := dep.CreatedAt; ms > 0 {
						out["deployed_at"] = time.UnixMilli(ms).UTC().Format(time.RFC3339)
					} else if ms := dep.Created; ms > 0 {
						out["deployed_at"] = time.UnixMilli(ms).UTC().Format(time.RFC3339)
					}
					if dep.InspectorURL != "" {
						out["inspect_url"] = dep.InspectorURL
						out["dashboard_url"] = vercelDashboardURL(dep.InspectorURL)
					}
				}
				cancel()
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
