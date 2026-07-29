package main

import (
	monitordomain "github.com/karoz/karoz/internal/monitor"
)

type monitorRegistrySnapshot struct {
	SchemaVersion     int                                           `json:"schema_version"`
	Monitors          map[string][]Monitor                          `json:"monitors"`
	ProbeReservations map[string]monitorProbeReservation            `json:"probe_reservations,omitempty"`
	ProbeChallenges   map[string]monitorProbeChallenge              `json:"probe_challenges,omitempty"`
	ProbeReceipts     map[string]monitordomain.ProbeApprovalReceipt `json:"probe_receipts,omitempty"`
	ProbeSessions     map[string]monitorProbeOperatorSession        `json:"probe_sessions,omitempty"`
}

func publicMonitor(item Monitor) Monitor {
	item.Trigger.ProbePath = ""
	item.Trigger.ApprovalReceiptID = ""
	return item
}

func publicMonitors(items []Monitor) []Monitor {
	out := make([]Monitor, len(items))
	for index := range items {
		out[index] = publicMonitor(items[index])
	}
	return out
}
