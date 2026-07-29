package main

import (
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func requireProcessMutationBoundary(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || !loopbackProcessOrigin(origin) ||
		!requestOriginMatchesRequest(origin, r) {
		writeError(
			w,
			http.StatusForbidden,
			errors.New("process mutations require a matching loopback Origin"),
		)
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeError(
			w,
			http.StatusUnsupportedMediaType,
			errors.New("process mutations require Content-Type application/json"),
		)
		return false
	}
	return true
}

func loopbackProcessOrigin(rawOrigin string) bool {
	origin, err := url.Parse(rawOrigin)
	if err != nil {
		return false
	}
	host := strings.TrimSpace(origin.Hostname())
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *app) handleProcesses(
	w http.ResponseWriter,
	r *http.Request,
	project Project,
	parts []string,
) {
	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		limit, err := processQueryInt(r, "limit", 20, 1, 100)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		processes, err := a.processViews(project.ID, "", limit)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, map[string]any{"processes": processes})
		return
	}

	processID := strings.TrimSpace(parts[0])
	if processID == "" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		process, err := a.processView(project.ID, "", processID)
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, errProcessNotFound) {
				status = http.StatusNotFound
				err = errors.New("process not found")
			}
			writeError(w, status, err)
			return
		}
		writeJSON(w, map[string]any{"process": process})
		return
	}
	if len(parts) == 2 && parts[1] == "log" &&
		r.Method == http.MethodGet {
		offset, err := processQueryInt(
			r,
			"offset",
			0,
			0,
			int(^uint(0)>>1),
		)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		limit, err := processQueryInt(
			r,
			"limit",
			50,
			1,
			maxProcessLogWindowLines,
		)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		tail, err := processQueryBool(r, "tail", r.URL.Query().Get("offset") == "")
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		window, err := a.readProcessLog(
			project.ID,
			processID,
			offset,
			limit,
			tail,
		)
		if err != nil {
			if errors.Is(err, errProcessRuntimeUnavailable) {
				writeError(w, http.StatusServiceUnavailable, errProcessRuntimeUnavailable)
				return
			}
			if errors.Is(err, errProcessLogGone) {
				writeError(w, http.StatusGone, err)
				return
			}
			if _, lookupErr := a.processRecord(project.ID, processID); errors.Is(
				lookupErr,
				errProcessNotFound,
			) {
				writeError(w, http.StatusNotFound, errors.New("process not found"))
				return
			} else if lookupErr != nil {
				writeError(
					w,
					http.StatusServiceUnavailable,
					errors.New("process runtime is unavailable"),
				)
				return
			}
			if strings.Contains(err.Error(), "offset exceeds bounded scan window") {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeError(
				w,
				http.StatusInternalServerError,
				errors.New("process log is unavailable"),
			)
			return
		}
		writeJSON(w, map[string]any{"log": window})
		return
	}
	if len(parts) == 2 && parts[1] == "stop" &&
		r.Method == http.MethodPost {
		process, err := a.stopProcess(project.ID, "", processID)
		if err != nil {
			status := http.StatusConflict
			if errors.Is(err, errProcessRuntimeUnavailable) {
				status = http.StatusServiceUnavailable
				err = errProcessRuntimeUnavailable
			} else if errors.Is(err, errProcessNotFound) {
				status = http.StatusNotFound
				err = errors.New("process not found")
			}
			writeError(w, status, err)
			return
		}
		writeJSON(w, map[string]any{"process": process})
		return
	}
	http.NotFound(w, r)
}

func processQueryInt(
	r *http.Request,
	key string,
	defaultValue, minimum, maximum int,
) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < int64(minimum) || value > int64(maximum) {
		return 0, errors.New(key + " is out of range")
	}
	return int(value), nil
}

func processQueryBool(
	r *http.Request,
	key string,
	defaultValue bool,
) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New(key + " must be true or false")
	}
	return value, nil
}
