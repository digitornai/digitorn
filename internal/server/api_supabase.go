package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/digitornai/digitorn/internal/server/mcpoauth"
	"github.com/go-chi/chi/v5"
)

const (
	supabaseProviderKey  = "supabase"
	supabaseMgmtProvider = "supabase-mgmt"
	supabaseOAuthAppID   = "@supabase-oauth"
)

var supabaseAPIBase = "https://api.supabase.com"

func (d *Daemon) supabaseOAuthCreds(ctx context.Context) (clientID, clientSecret string) {
	if d.mcpHub != nil {
		if sys, err := d.mcpHub.PiecesSystemConfig(ctx, supabaseProviderKey); err == nil && sys != nil {
			clientID, _ = sys.DigitornProvided["oauth_client_id"].(string)
			clientSecret, _ = sys.DigitornProvided["oauth_client_secret"].(string)
		}
	}
	if clientID == "" {
		clientID = os.Getenv("DIGITORN_SUPABASE_CLIENT_ID")
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("DIGITORN_SUPABASE_CLIENT_SECRET")
	}
	return clientID, clientSecret
}

func (d *Daemon) supabaseOAuthStart(w http.ResponseWriter, r *http.Request) {
	if d.mcpOAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth_unavailable", "OAuth is not configured on this daemon.")
		return
	}
	clientID, _ := d.supabaseOAuthCreds(r.Context())
	if clientID == "" {
		writeError(w, http.StatusServiceUnavailable, "supabase_oauth_unconfigured",
			"Supabase OAuth app is not provisioned (client_id missing).")
		return
	}
	redirectURI := d.cfg.OAuth.PieceRedirectURL
	state, err := d.mcpOAuth.MintState(r.Context(), userIDOf(r.Context()), supabaseOAuthAppID, supabaseProviderKey, supabaseProviderKey, redirectURI)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oauth_start_failed", err.Error())
		return
	}
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("state", state)
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url": supabaseAPIBase + "/v1/oauth/authorize?" + q.Encode(),
		"state":    state,
	})
}

func supabaseExchangeCode(ctx context.Context, clientID, clientSecret, code, redirectURI string) (access, refresh string, expiresAt int64, err error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, supabaseAPIBase+"/v1/oauth/token", strings.NewReader(form.Encode()))
	if rerr != nil {
		return "", "", 0, rerr
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(clientID, clientSecret)
	resp, derr := http.DefaultClient.Do(req)
	if derr != nil {
		return "", "", 0, derr
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, fmt.Errorf("supabase token endpoint: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		return "", "", 0, uerr
	}
	if out.AccessToken == "" {
		return "", "", 0, errors.New("supabase: token response had no access_token")
	}
	if out.ExpiresIn > 0 {
		expiresAt = time.Now().UTC().Unix() + out.ExpiresIn
	}
	return out.AccessToken, out.RefreshToken, expiresAt, nil
}

func (d *Daemon) supabaseCompleteOAuth(ctx context.Context, userID, code, redirectURI string) error {
	clientID, clientSecret := d.supabaseOAuthCreds(ctx)
	if clientID == "" || clientSecret == "" {
		return errors.New("supabase oauth not configured")
	}
	access, refresh, expiresAt, err := supabaseExchangeCode(ctx, clientID, clientSecret, code, redirectURI)
	if err != nil {
		return err
	}
	if d.mcpOAuth == nil {
		return errors.New("oauth service unavailable")
	}
	return d.mcpOAuth.SetManual(ctx, userID, supabaseMgmtProvider, &mcpoauth.Token{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresAt:    expiresAt,
	})
}

func (d *Daemon) supabaseMgmtToken(r *http.Request) (string, error) {
	if d.mcpOAuth == nil {
		return "", errors.New("supabase not connected")
	}
	tok, err := d.mcpOAuth.GetToken(r.Context(), userIDOf(r.Context()), supabaseMgmtProvider)
	if err != nil || tok == nil || tok.AccessToken == "" {
		return "", errors.New("supabase not connected")
	}
	return tok.AccessToken, nil
}

func supabaseRequest(ctx context.Context, method, path, token string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, supabaseAPIBase+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
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

type supabaseProject struct {
	ID     string `json:"id"`
	Ref    string `json:"ref"`
	Name   string `json:"name"`
	Region string `json:"region"`
	Status string `json:"status"`
}

func supabaseProjectURL(ref string) string { return "https://" + ref + ".supabase.co" }

func (d *Daemon) supabaseProjects(w http.ResponseWriter, r *http.Request) {
	token, err := d.supabaseMgmtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "supabase_not_connected", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	data, code, rerr := supabaseRequest(ctx, http.MethodGet, "/v1/projects", token, nil)
	if rerr != nil || code < 200 || code >= 300 {
		writeError(w, supabaseErrStatus(code), supabaseErrCode(code), supabaseErrMessage(data, rerr, "could not list Supabase projects"))
		return
	}
	var projects []supabaseProject
	_ = json.Unmarshal(data, &projects)
	out := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		ref := p.Ref
		if ref == "" {
			ref = p.ID
		}
		out = append(out, map[string]any{
			"ref": ref, "name": p.Name, "region": p.Region, "status": p.Status,
			"url": supabaseProjectURL(ref),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func supabaseAnonKey(ctx context.Context, token, ref string) (string, error) {
	data, code, err := supabaseRequest(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(ref)+"/api-keys", token, nil)
	if err != nil {
		return "", err
	}
	if code < 200 || code >= 300 {
		return "", errors.New(supabaseErrMessage(data, nil, fmt.Sprintf("supabase api-keys: HTTP %d", code)))
	}
	var keys []struct {
		Name   string `json:"name"`
		APIKey string `json:"api_key"`
	}
	if uerr := json.Unmarshal(data, &keys); uerr != nil {
		return "", uerr
	}
	for _, k := range keys {
		if strings.EqualFold(k.Name, "anon") {
			return k.APIKey, nil
		}
	}
	if len(keys) > 0 {
		return keys[0].APIKey, nil
	}
	return "", errors.New("supabase: no api key returned")
}

func (d *Daemon) supabaseStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"connected": false, "linked": false}
	if d.mcpOAuth != nil {
		if tok, err := d.mcpOAuth.GetToken(r.Context(), userIDOf(r.Context()), supabaseMgmtProvider); err == nil && tok != nil && tok.AccessToken != "" {
			out["connected"] = true
			// Lets the client tell a fresh authorization from the existing one:
			// re-connecting keeps "connected" true, only the token changes.
			out["expires_at"] = tok.ExpiresAt
		}
	}
	if pm := d.piecesModule(); pm != nil && pm.PiecesStore() != nil {
		if wire, werr := pm.PiecesStore().RevealAuth(r.Context(), userIDOf(r.Context()), supabaseProviderKey); werr == nil && wire != nil {
			if u := wire.Fields["url"]; u != "" {
				out["linked"] = true
				out["url"] = u
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Daemon) supabaseConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Ref) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "project ref is required")
		return
	}
	ref := strings.TrimSpace(req.Ref)
	token, err := d.supabaseMgmtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "supabase_not_connected", err.Error())
		return
	}
	pm := d.piecesModule()
	if pm == nil || pm.PiecesStore() == nil {
		writeError(w, http.StatusServiceUnavailable, "pieces_unavailable", "pieces store is not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	key, kerr := supabaseAnonKey(ctx, token, ref)
	if kerr != nil {
		if supabaseIsPaused(supabaseProjectStatus(ctx, token, ref)) {
			writeError(w, http.StatusConflict, "supabase_project_paused",
				"This Supabase project is paused, so it has no API keys yet. Restore it from your Supabase dashboard, then connect it again.")
			return
		}
		writeError(w, http.StatusBadGateway, "supabase_error", kerr.Error())
		return
	}
	projURL := supabaseProjectURL(ref)
	uid := userIDOf(r.Context())
	if ierr := pm.PiecesStore().Install(ctx, uid, supabaseProviderKey, "", "custom",
		map[string]string{"url": projURL, "apiKey": key}); ierr != nil {
		writeError(w, http.StatusInternalServerError, "connect_failed", ierr.Error())
		return
	}
	d.piecesCatalog.invalidate("")
	injected := d.supabaseInjectIntoVercel(ctx, r, projURL, key)
	writeJSON(w, http.StatusOK, map[string]any{
		"linked": true, "url": projURL, "env_injected": injected,
	})
}

// supabaseInjectIntoVercel pushes the project's URL and anon key to the Vercel
// project as build-time env vars so the published site can reach the database.
// Best-effort: publishing may not have happened yet.
func (d *Daemon) supabaseInjectIntoVercel(ctx context.Context, r *http.Request, projURL, anonKey string) bool {
	wd, err := d.sessionWorkdir(r.Context(), chi.URLParam(r, "session_id"))
	if err != nil || wd == "" {
		return false
	}
	st := readVercelState(wd)
	if st == nil || st.ProjectName == "" {
		return false
	}
	token, teamID, aerr := d.vercelAuth(r)
	if aerr != nil {
		return false
	}
	ok := true
	for key, value := range map[string]string{
		"VITE_SUPABASE_URL":      projURL,
		"VITE_SUPABASE_ANON_KEY": anonKey,
	} {
		body := map[string]any{
			"key": key, "value": value, "type": "encrypted",
			"target": []string{"production", "preview", "development"},
		}
		_, code, rerr := vercelRequest(ctx, http.MethodPost,
			"/v10/projects/"+url.PathEscape(st.ProjectName)+"/env?upsert=true", token, teamID, body)
		if rerr != nil || code < 200 || code >= 300 {
			ok = false
		}
	}
	return ok
}

func supabaseErrMessage(data []byte, err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil {
		if e.Message != "" {
			return e.Message
		}
		if e.Error != "" {
			return e.Error
		}
	}
	return fallback
}

func supabaseErrStatus(code int) int {
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		return http.StatusForbidden
	}
	return http.StatusBadGateway
}

func supabaseErrCode(code int) string {
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		return "supabase_scope_missing"
	}
	return "supabase_error"
}

func supabaseProjectStatus(ctx context.Context, token, ref string) string {
	data, code, err := supabaseRequest(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(ref), token, nil)
	if err != nil || code < 200 || code >= 300 {
		return ""
	}
	var p supabaseProject
	if json.Unmarshal(data, &p) != nil {
		return ""
	}
	return p.Status
}

// supabaseIsPaused reports whether a project cannot serve API keys yet.
//
// It deliberately treats every non-healthy state as not-ready instead of
// listing the paused ones: an allow-list silently missed RESTORING and
// COMING_UP, so a project that was still waking up looked ready, its
// key list came back empty, and the connector was linked with no
// credentials. Supabase is free to add states; only ACTIVE_HEALTHY means
// "usable". An empty status carries no information and is left alone so a
// probe failure never triggers a restore loop. This mirrors the client,
// which gates the same decision on ACTIVE_HEALTHY.
func supabaseIsPaused(status string) bool {
	s := strings.ToUpper(strings.TrimSpace(status))
	return s != "" && s != "ACTIVE_HEALTHY"
}

func (d *Daemon) supabaseRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref string `json:"ref"`
	}
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.Ref) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "project ref is required")
		return
	}
	ref := strings.TrimSpace(req.Ref)
	token, err := d.supabaseMgmtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "supabase_not_connected", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	data, code, rerr := supabaseRequest(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(ref)+"/restore", token, map[string]any{})
	if rerr != nil || code < 200 || code >= 300 {
		writeError(w, supabaseErrStatus(code), supabaseErrCode(code),
			supabaseErrMessage(data, rerr, "could not restore the Supabase project"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restoring": true, "status": supabaseProjectStatus(ctx, token, ref)})
}

// supabaseOrgAndRegion reuses the org (and region) of an existing project so a
// new database lands next to the user, without needing the organizations scope.
func supabaseOrgAndRegion(ctx context.Context, token string) (org, region string) {
	if data, code, err := supabaseRequest(ctx, http.MethodGet, "/v1/projects", token, nil); err == nil && code >= 200 && code < 300 {
		var projects []struct {
			OrgSlug string `json:"organization_slug"`
			OrgID   string `json:"organization_id"`
			Region  string `json:"region"`
		}
		if json.Unmarshal(data, &projects) == nil {
			for _, p := range projects {
				if org == "" {
					if p.OrgSlug != "" {
						org = p.OrgSlug
					} else if p.OrgID != "" {
						org = p.OrgID
					}
				}
				if region == "" && p.Region != "" {
					region = p.Region
				}
			}
		}
	}
	if org != "" {
		return org, region
	}
	return supabaseOrgSlug(ctx, token), region
}

func supabaseOrgSlug(ctx context.Context, token string) string {
	if data, code, err := supabaseRequest(ctx, http.MethodGet, "/v1/organizations", token, nil); err == nil && code >= 200 && code < 300 {
		var orgs []struct {
			Slug string `json:"slug"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(data, &orgs) == nil && len(orgs) > 0 {
			if orgs[0].Slug != "" {
				return orgs[0].Slug
			}
			return orgs[0].ID
		}
	}
	return ""
}

func supabaseDBPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "Dg" + base64.RawURLEncoding.EncodeToString(b), nil
}

func (d *Daemon) supabaseCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	_ = readJSONLenient(r, &req)
	token, err := d.supabaseMgmtToken(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "supabase_not_connected", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	org, region := supabaseOrgAndRegion(ctx, token)
	if region == "" {
		region = "us-east-1"
	}
	if org == "" {
		writeError(w, http.StatusConflict, "supabase_no_org",
			"Could not find your Supabase organization. Add the “Organizations: Read” permission to the Digitorn app in Supabase, then reconnect.")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = publishRepoName(r)
	}
	pass, perr := supabaseDBPassword()
	if perr != nil {
		writeError(w, http.StatusInternalServerError, "internal", perr.Error())
		return
	}
	body := map[string]any{
		"name":              name,
		"organization_slug": org,
		"db_pass":           pass,
		"region":            region,
		"plan":              "free",
	}
	data, code, rerr := supabaseRequest(ctx, http.MethodPost, "/v1/projects", token, body)
	if rerr != nil || code < 200 || code >= 300 {
		writeError(w, supabaseErrStatus(code), supabaseErrCode(code),
			supabaseErrMessage(data, rerr, "could not create the Supabase project"))
		return
	}
	var p supabaseProject
	_ = json.Unmarshal(data, &p)
	ref := p.Ref
	if ref == "" {
		ref = p.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref": ref, "name": p.Name, "status": p.Status, "url": supabaseProjectURL(ref),
	})
}
