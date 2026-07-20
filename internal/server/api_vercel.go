package server

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
	pm := d.piecesModule()
	if pm == nil || pm.PiecesStore() == nil {
		return "", "", errors.New("vercel not connected")
	}
	wire, werr := pm.PiecesStore().RevealAuth(r.Context(), userIDOf(r.Context()), vercelProviderKey)
	if werr != nil || wire == nil {
		return "", "", errors.New("vercel not connected")
	}
	token = wire.Fields["token"]
	if token == "" {
		token = wire.Value
	}
	teamID = wire.Fields["teamId"]
	if strings.TrimSpace(token) == "" {
		return "", "", errors.New("vercel not connected")
	}
	return token, teamID, nil
}

const vercelOAuthAppID = "@vercel-oauth"

func (d *Daemon) vercelOAuthStart(w http.ResponseWriter, r *http.Request) {
	if d.mcpOAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth_unavailable", "OAuth is not configured on this daemon.")
		return
	}
	clientID, _, slug := d.vercelOAuthCreds(r.Context())
	if clientID == "" || slug == "" {
		writeError(w, http.StatusServiceUnavailable, "vercel_oauth_unconfigured",
			"Vercel OAuth integration is not provisioned (client_id/slug missing).")
		return
	}
	redirectURI := d.cfg.OAuth.PieceRedirectURL
	state, err := d.mcpOAuth.MintState(r.Context(), userIDOf(r.Context()), vercelOAuthAppID, vercelProviderKey, vercelProviderKey, redirectURI)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oauth_start_failed", err.Error())
		return
	}
	authURL := fmt.Sprintf("https://vercel.com/integrations/%s/new?state=%s", url.PathEscape(slug), url.QueryEscape(state))
	writeJSON(w, http.StatusOK, map[string]any{"auth_url": authURL, "state": state})
}

func vercelExchangeCode(ctx context.Context, clientID, clientSecret, code, redirectURI string) (token, teamID string, err error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vercelAPIBase+"/v2/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("vercel token endpoint: %d %s", resp.StatusCode, string(body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TeamID      string `json:"team_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", err
	}
	if out.AccessToken == "" {
		return "", "", errors.New("vercel: token response had no access_token")
	}
	return out.AccessToken, out.TeamID, nil
}

func (d *Daemon) vercelCompleteOAuth(ctx context.Context, userID, code, redirectURI string) error {
	clientID, clientSecret, _ := d.vercelOAuthCreds(ctx)
	if clientID == "" || clientSecret == "" {
		return errors.New("vercel oauth not configured")
	}
	token, teamID, err := vercelExchangeCode(ctx, clientID, clientSecret, code, redirectURI)
	if err != nil {
		return err
	}
	pm := d.piecesModule()
	if pm == nil || pm.PiecesStore() == nil {
		return errors.New("pieces store unavailable")
	}
	creds := map[string]string{"token": token}
	if teamID != "" {
		creds["teamId"] = teamID
	}
	if err := pm.PiecesStore().Install(ctx, userID, vercelProviderKey, "", "custom", creds); err != nil {
		return err
	}
	d.piecesCatalog.invalidate("")
	return nil
}

func (d *Daemon) vercelOAuthCreds(ctx context.Context) (clientID, clientSecret, slug string) {
	if d.mcpHub != nil {
		if sys, err := d.mcpHub.PiecesSystemConfig(ctx, vercelProviderKey); err == nil && sys != nil {
			clientID, _ = sys.DigitornProvided["oauth_client_id"].(string)
			clientSecret, _ = sys.DigitornProvided["oauth_client_secret"].(string)
			slug, _ = sys.DigitornProvided["oauth_slug"].(string)
		}
	}
	if clientID == "" {
		clientID = os.Getenv("DIGITORN_VERCEL_CLIENT_ID")
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("DIGITORN_VERCEL_CLIENT_SECRET")
	}
	if slug == "" {
		slug = os.Getenv("DIGITORN_VERCEL_SLUG")
	}
	return clientID, clientSecret, slug
}

func (d *Daemon) vercelConnect(w http.ResponseWriter, r *http.Request) {
	pm := d.piecesModule()
	if pm == nil || pm.PiecesStore() == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces store is not available")
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
	creds := map[string]string{"token": strings.TrimSpace(req.Token)}
	if strings.TrimSpace(req.TeamID) != "" {
		creds["teamId"] = strings.TrimSpace(req.TeamID)
	}
	if err := pm.PiecesStore().Install(r.Context(), userIDOf(r.Context()), vercelProviderKey, "", "custom", creds); err != nil {
		writeError(w, http.StatusInternalServerError, "connect_failed", err.Error())
		return
	}
	d.piecesCatalog.invalidate("")
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
	if !strings.Contains(m, "github") {
		return false
	}
	// "login connection" is the wording Vercel actually returns when the
	// account is not linked; matching only install/integration let the real
	// message fall through to a raw error the user could not act on.
	return strings.Contains(m, "install") ||
		strings.Contains(m, "integration") ||
		strings.Contains(m, "login connection")
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
	return "https://" + vercelPublicURL(name, dep.Alias), nil
}

func publishRepoName(r *http.Request) string {
	base := chi.URLParam(r, "app_id")
	if base == "" {
		base = "digitorn-app"
	}
	sid := chi.URLParam(r, "session_id")
	if len(sid) > 8 {
		sid = sid[:8]
	}
	name := base
	if sid != "" {
		name = base + "-" + sid
	}
	name = vercelNameRe.ReplaceAllString(strings.ToLower(name), "-")
	return strings.Trim(name, "-._")
}

type deployFile struct {
	Path string
	SHA  string
	Size int
	Data []byte
}

var deployExcludeDirs = map[string]bool{
	"node_modules": true, ".git": true, ".digitorn": true, ".vercel": true,
	"dist": true, "build": true, ".next": true, ".cache": true, ".turbo": true,
	".svelte-kit": true, ".output": true, "out": true,
}

func collectDeployFiles(wd string) ([]deployFile, error) {
	var files []deployFile
	err := filepath.WalkDir(wd, func(p string, ent fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(wd, p)
		if rerr != nil || rel == "." {
			return nil
		}
		if ent.IsDir() {
			if deployExcludeDirs[ent.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := ent.Info()
		if ierr != nil {
			return ierr
		}
		if !info.Mode().IsRegular() || info.Size() > 25<<20 {
			return nil
		}
		data, derr := os.ReadFile(p)
		if derr != nil {
			return derr
		}
		sum := sha1.Sum(data)
		files = append(files, deployFile{
			Path: filepath.ToSlash(rel),
			SHA:  hex.EncodeToString(sum[:]),
			Size: len(data),
			Data: data,
		})
		return nil
	})
	return files, err
}

func vercelUploadFiles(ctx context.Context, token, teamID string, files []deployFile) error {
	for _, f := range files {
		if err := vercelUploadFile(ctx, token, teamID, f); err != nil {
			return err
		}
	}
	return nil
}

func vercelUploadFile(ctx context.Context, token, teamID string, f deployFile) error {
	u := vercelAPIBase + "/v2/files"
	if teamID != "" {
		u += "?teamId=" + url.QueryEscape(teamID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(f.Data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("x-vercel-digest", f.SHA)
	req.ContentLength = int64(len(f.Data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload %s: HTTP %d %s", f.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func detectFramework(wd string) string {
	raw, err := os.ReadFile(filepath.Join(wd, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return ""
	}
	has := func(dep string) bool {
		if _, ok := pkg.Dependencies[dep]; ok {
			return true
		}
		_, ok := pkg.DevDependencies[dep]
		return ok
	}
	switch {
	case has("next"):
		return "nextjs"
	case has("vite"):
		return "vite"
	case has("react-scripts"):
		return "create-react-app"
	case has("@angular/core"):
		return "angular"
	case has("@sveltejs/kit"):
		return "sveltekit"
	case has("nuxt"):
		return "nuxtjs"
	case has("gatsby"):
		return "gatsby"
	default:
		return ""
	}
}

func vercelDeployProjectName(r *http.Request, wd string) string {
	if vst := readVercelState(wd); vst != nil && vst.ProjectName != "" {
		return vst.ProjectName
	}
	return publishRepoName(r)
}

func vercelPublicURL(name string, _ []string) string {
	return name + ".vercel.app"
}

func vercelCreateFileDeployment(ctx context.Context, token, teamID, name string, files []deployFile, framework string) (string, error) {
	manifest := make([]map[string]any, 0, len(files))
	for _, f := range files {
		manifest = append(manifest, map[string]any{"file": f.Path, "sha": f.SHA, "size": f.Size})
	}
	settings := map[string]any{"framework": nil}
	if framework != "" {
		settings["framework"] = framework
	}
	body := map[string]any{
		"name":            name,
		"files":           manifest,
		"target":          "production",
		"projectSettings": settings,
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
	return "https://" + vercelPublicURL(name, dep.Alias), nil
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
	token, teamID, err := d.vercelAuth(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "vercel_not_connected", "connect Vercel to publish")
		return
	}
	ctx, cancel := longGitOp(w, r, 15*time.Minute)
	defer cancel()

	files, ferr := collectDeployFiles(wd)
	if ferr != nil {
		writeError(w, http.StatusInternalServerError, "collect_failed", ferr.Error())
		return
	}
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "empty_workspace", "nothing to publish — the workspace has no files yet")
		return
	}
	if uerr := vercelUploadFiles(ctx, token, teamID, files); uerr != nil {
		writeError(w, http.StatusBadGateway, "vercel_upload_failed", uerr.Error())
		return
	}
	name := vercelDeployProjectName(r, wd)
	liveURL, derr := vercelCreateFileDeployment(ctx, token, teamID, name, files, detectFramework(wd))
	if derr != nil {
		writeError(w, http.StatusBadGateway, "vercel_deploy_failed", derr.Error())
		return
	}
	_ = writeVercelState(wd, &vercelState{
		ProjectName: name,
		URL:         liveURL,
		TeamID:      teamID,
		DeployedAt:  time.Now().UTC().Format(time.RFC3339),
	})
	writeJSON(w, http.StatusOK, map[string]any{"url": liveURL, "project": name})
}

type vercelDeployment struct {
	UID          string `json:"uid"`
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
			pid := st.ProjectID
			if pid == "" {
				pid = st.ProjectName
			}
			if connected && pid != "" {
				token, teamID, _ := d.vercelAuth(r)
				ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
				if dep := vercelLatestDeployment(ctx, token, teamID, pid); dep != nil {
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

func (d *Daemon) vercelLogs(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "session_id")
	wd, err := d.sessionWorkdir(r.Context(), sid)
	if err != nil {
		writeError(w, errStatus(err), errCode(err), err.Error())
		return
	}
	st := readVercelState(wd)
	if st == nil || (st.ProjectID == "" && st.ProjectName == "") {
		writeError(w, http.StatusConflict, "not_deployed", "nothing has been deployed yet")
		return
	}
	pid := st.ProjectID
	if pid == "" {
		pid = st.ProjectName
	}
	token, teamID, err := d.vercelAuth(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "vercel_not_connected", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	dep := vercelLatestDeployment(ctx, token, teamID, pid)
	if dep == nil || dep.UID == "" {
		writeError(w, http.StatusBadGateway, "vercel_error", "could not fetch the latest deployment")
		return
	}
	data, code, err := vercelRequest(ctx, http.MethodGet, "/v3/deployments/"+url.PathEscape(dep.UID)+"/events?builds=1&limit=1000", token, teamID, nil)
	if err != nil || code < 200 || code >= 300 {
		writeError(w, http.StatusBadGateway, "vercel_error", "could not fetch build logs")
		return
	}
	var events []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Payload json.RawMessage `json:"payload"`
	}
	_ = json.Unmarshal(data, &events)
	type logLine struct {
		Text  string `json:"text"`
		Error bool   `json:"error"`
	}
	lines := make([]logLine, 0, len(events))
	for _, ev := range events {
		txt := ev.Text
		if txt == "" && len(ev.Payload) > 0 {
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(ev.Payload, &p)
			txt = p.Text
		}
		txt = strings.TrimRight(txt, "\r\n")
		if strings.TrimSpace(txt) == "" {
			continue
		}
		lines = append(lines, logLine{Text: txt, Error: ev.Type == "stderr" || ev.Type == "error"})
	}
	state := dep.ReadyState
	if state == "" {
		state = dep.State
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "lines": lines})
}

type vercelEnvView struct {
	ID     string   `json:"id"`
	Key    string   `json:"key"`
	Target []string `json:"target"`
	Type   string   `json:"type"`
}

func (d *Daemon) vercelEnvProject(r *http.Request) (name, token, teamID string, code int, msg string) {
	wd, err := d.sessionWorkdir(r.Context(), chi.URLParam(r, "session_id"))
	if err != nil || wd == "" {
		return "", "", "", http.StatusBadRequest, "session has no workdir"
	}
	st := readVercelState(wd)
	if st == nil || st.ProjectName == "" {
		return "", "", "", http.StatusConflict, "publish once before managing environment variables"
	}
	token, teamID, err = d.vercelAuth(r)
	if err != nil {
		return "", "", "", http.StatusUnauthorized, "connect Vercel first"
	}
	return st.ProjectName, token, teamID, 0, ""
}

func (d *Daemon) vercelEnvList(w http.ResponseWriter, r *http.Request) {
	name, token, teamID, code, msg := d.vercelEnvProject(r)
	if code != 0 {
		if code == http.StatusConflict {
			writeJSON(w, http.StatusOK, map[string]any{"env": []any{}})
			return
		}
		writeError(w, code, "vercel_env", msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	data, hc, err := vercelRequest(ctx, http.MethodGet, "/v9/projects/"+url.PathEscape(name)+"/env?decrypt=false", token, teamID, nil)
	if err != nil || hc < 200 || hc >= 300 {
		writeError(w, http.StatusBadGateway, "vercel_error", "could not fetch environment variables")
		return
	}
	var resp struct {
		Envs []vercelEnvView `json:"envs"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Envs == nil {
		resp.Envs = []vercelEnvView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"env": resp.Envs})
}

func (d *Daemon) vercelEnvSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key    string   `json:"key"`
		Value  string   `json:"value"`
		Target []string `json:"target"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Key) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "key is required")
		return
	}
	name, token, teamID, code, msg := d.vercelEnvProject(r)
	if code != 0 {
		writeError(w, code, "vercel_env", msg)
		return
	}
	target := req.Target
	if len(target) == 0 {
		target = []string{"production", "preview", "development"}
	}
	body := map[string]any{
		"key":    strings.TrimSpace(req.Key),
		"value":  req.Value,
		"type":   "encrypted",
		"target": target,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	data, hc, err := vercelRequest(ctx, http.MethodPost, "/v10/projects/"+url.PathEscape(name)+"/env?upsert=true", token, teamID, body)
	if err != nil || hc < 200 || hc >= 300 {
		m := "could not set the environment variable"
		if e := vercelParseErr(data); e != nil && e.Error.Message != "" {
			m = e.Error.Message
		}
		writeError(w, http.StatusBadGateway, "vercel_error", m)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (d *Daemon) vercelEnvDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "env id is required")
		return
	}
	name, token, teamID, code, msg := d.vercelEnvProject(r)
	if code != 0 {
		writeError(w, code, "vercel_env", msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, hc, err := vercelRequest(ctx, http.MethodDelete, "/v9/projects/"+url.PathEscape(name)+"/env/"+url.PathEscape(id), token, teamID, nil)
	if err != nil || hc < 200 || hc >= 300 {
		writeError(w, http.StatusBadGateway, "vercel_error", "could not delete the environment variable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type vercelDNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type vercelDomainView struct {
	Name         string            `json:"name"`
	Verified     bool              `json:"verified"`
	Verification []vercelDNSRecord `json:"verification,omitempty"`
	DNS          []vercelDNSRecord `json:"dns,omitempty"`
}

// vercelDomainDNS returns the DNS records the user must add to point a domain at
// Vercel: an apex domain uses an A record, a subdomain a CNAME.
func vercelDomainDNS(domain string) []vercelDNSRecord {
	labels := strings.Split(strings.TrimSuffix(domain, "."), ".")
	if len(labels) <= 2 {
		return []vercelDNSRecord{{Type: "A", Name: "@", Value: "76.76.21.21"}}
	}
	sub := strings.Join(labels[:len(labels)-2], ".")
	return []vercelDNSRecord{{Type: "CNAME", Name: sub, Value: "cname.vercel-dns.com"}}
}

func (d *Daemon) vercelDomainList(w http.ResponseWriter, r *http.Request) {
	name, token, teamID, code, msg := d.vercelEnvProject(r)
	if code != 0 {
		if code == http.StatusConflict {
			writeJSON(w, http.StatusOK, map[string]any{"domains": []any{}})
			return
		}
		writeError(w, code, "vercel_domain", msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	data, hc, err := vercelRequest(ctx, http.MethodGet, "/v9/projects/"+url.PathEscape(name)+"/domains", token, teamID, nil)
	if err != nil || hc < 200 || hc >= 300 {
		writeError(w, http.StatusBadGateway, "vercel_error", "could not fetch domains")
		return
	}
	var resp struct {
		Domains []vercelDomainView `json:"domains"`
	}
	_ = json.Unmarshal(data, &resp)
	out := make([]vercelDomainView, 0, len(resp.Domains))
	for _, dm := range resp.Domains {
		if strings.HasSuffix(dm.Name, ".vercel.app") {
			continue
		}
		if !dm.Verified {
			dm.DNS = vercelDomainDNS(dm.Name)
		}
		out = append(out, dm)
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": out})
}

func (d *Daemon) vercelDomainAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "domain name is required")
		return
	}
	domain := strings.ToLower(strings.TrimSpace(req.Name))
	name, token, teamID, code, msg := d.vercelEnvProject(r)
	if code != 0 {
		writeError(w, code, "vercel_domain", msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	data, hc, err := vercelRequest(ctx, http.MethodPost, "/v10/projects/"+url.PathEscape(name)+"/domains", token, teamID, map[string]any{"name": domain})
	if err != nil || hc < 200 || hc >= 300 {
		m := "could not add the domain"
		if e := vercelParseErr(data); e != nil && e.Error.Message != "" {
			m = e.Error.Message
		}
		writeError(w, http.StatusBadGateway, "vercel_error", m)
		return
	}
	var dm vercelDomainView
	_ = json.Unmarshal(data, &dm)
	if dm.Name == "" {
		dm.Name = domain
	}
	if !dm.Verified {
		dm.DNS = vercelDomainDNS(dm.Name)
	}
	writeJSON(w, http.StatusOK, dm)
}

func (d *Daemon) vercelDomainRemove(w http.ResponseWriter, r *http.Request) {
	domain := chi.URLParam(r, "domain")
	if domain == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "domain is required")
		return
	}
	name, token, teamID, code, msg := d.vercelEnvProject(r)
	if code != 0 {
		writeError(w, code, "vercel_domain", msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, hc, err := vercelRequest(ctx, http.MethodDelete, "/v9/projects/"+url.PathEscape(name)+"/domains/"+url.PathEscape(domain), token, teamID, nil)
	if err != nil || hc < 200 || hc >= 300 {
		writeError(w, http.StatusBadGateway, "vercel_error", "could not remove the domain")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
