package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// requirementsResponse is what both the status GET and the provision POST return.
type requirementsResponse struct {
	AppID        string `json:"app_id"`
	Requirements any    `json:"requirements"` // []provision.Status
}

// getRequirements — GET /api/apps/{app_id}/requirements. Returns the live status
// of every binary the app declares under `requirements:`. The client polls this
// on app open (empty list ⇒ no dialog) and while a download is in flight.
func (d *Daemon) getRequirements(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	def, err := d.appMgr.GetManifest(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "manifest_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, requirementsResponse{
		AppID:        appID,
		Requirements: d.provisioner.Statuses(def.Requirements),
	})
}

// provisionRequirements — POST /api/apps/{app_id}/requirements/provision. The
// user's explicit consent to download. Kicks off the async jobs and returns
// immediately with the (now queued) statuses; the client polls GET to follow
// progress. Nothing is ever downloaded without this call.
func (d *Daemon) provisionRequirements(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "app_id")
	def, err := d.appMgr.GetManifest(r.Context(), appID)
	if err != nil {
		writeError(w, appMgrErrStatus(err), "manifest_failed", err.Error())
		return
	}
	d.provisioner.Provision(appID, def.Requirements)
	writeJSON(w, http.StatusOK, requirementsResponse{
		AppID:        appID,
		Requirements: d.provisioner.Statuses(def.Requirements),
	})
}
