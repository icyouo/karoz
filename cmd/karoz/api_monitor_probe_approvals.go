package main

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

const monitorProbeSessionCookie = "karoz_monitor_probe_session"

func (a *app) handleMonitorProbeApprovals(
	w http.ResponseWriter,
	r *http.Request,
	project Project,
	parts []string,
) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sessionToken := ""
	if cookie, err := r.Cookie(monitorProbeSessionCookie); err == nil {
		sessionToken = cookie.Value
	}
	if len(parts) == 1 && parts[0] == "prepare" {
		var request monitorProbeApprovalRequest
		if err := readJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		challenge, rawSession, err := a.prepareMonitorProbeApproval(
			project,
			request,
			sessionToken,
		)
		if err != nil {
			writeError(w, probeApprovalHTTPStatus(err), err)
			return
		}
		if rawSession != sessionToken {
			http.SetCookie(w, &http.Cookie{
				Name:     monitorProbeSessionCookie,
				Value:    rawSession,
				Path:     "/api/projects/" + project.ID + "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				MaxAge:   int(monitorProbeSessionLifetime / time.Second),
			})
		}
		writeJSON(w, map[string]any{"challenge": challenge})
		return
	}
	if len(parts) == 2 && parts[1] == "confirm" {
		receipt, err := a.confirmMonitorProbeApproval(
			project,
			strings.TrimSpace(parts[0]),
			sessionToken,
		)
		if err != nil {
			writeError(w, probeApprovalHTTPStatus(err), err)
			return
		}
		writeJSON(w, map[string]any{
			"receipt":           receipt,
			"monitor_created":   false,
			"dev_turn_required": true,
		})
		return
	}
	http.NotFound(w, r)
}

func probeApprovalHTTPStatus(err error) int {
	if errors.Is(err, errScriptProbeUnsupported) {
		return http.StatusNotImplemented
	}
	return http.StatusBadRequest
}
