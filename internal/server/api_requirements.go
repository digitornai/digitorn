package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

type requirementsResponse struct {
	AppID        string `json:"app_id"`
	Requirements any    `json:"requirements"`
}

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
