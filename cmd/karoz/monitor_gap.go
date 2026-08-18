package main

import (
	"errors"
	"strings"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

type monitorGapAcknowledgement struct {
	AuthorityID        string `json:"authority_id"`
	SourceKind         string `json:"source_kind"`
	ExpectedGapVersion uint64 `json:"expected_gap_version"`
}

func (a *app) acknowledgeMonitorGap(
	project Project,
	monitorID string,
	request monitorGapAcknowledgement,
	principal string,
) (Monitor, error) {
	request.AuthorityID = strings.TrimSpace(request.AuthorityID)
	request.SourceKind = strings.TrimSpace(request.SourceKind)
	principal = strings.TrimSpace(principal)
	if request.AuthorityID == "" || request.SourceKind == "" ||
		request.ExpectedGapVersion == 0 || principal == "" {
		return Monitor{}, errors.New("source gap acknowledgement is incomplete")
	}

	a.agentRuntimeLocked().backgroundOwnerMu.Lock()
	defer a.agentRuntimeLocked().backgroundOwnerMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	items := a.monitors[project.ID]
	for index := range items {
		if items[index].ID != monitorID {
			continue
		}
		before := cloneMonitorList(items)
		key := monitordomain.SourceGapKey(
			request.AuthorityID,
			request.SourceKind,
		)
		gap, ok := items[index].SourceGaps[key]
		if !ok {
			return Monitor{}, errors.New("source gap not found")
		}
		updated, err := monitordomain.AcknowledgeSourceGap(
			items[index],
			request.AuthorityID,
			request.SourceKind,
			request.ExpectedGapVersion,
			gap.LastVersion,
			principal,
			time.Now().UTC(),
		)
		if err != nil {
			return Monitor{}, err
		}
		items[index] = updated
		a.monitors[project.ID] = items
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitors[project.ID] = before
			return Monitor{}, err
		}
		return updated, nil
	}
	return Monitor{}, errors.New("monitor not found")
}
