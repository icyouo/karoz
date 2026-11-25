package collaboration

import "strings"

// CanonicalMessageStatus maps commercial resident_agent_messages.status
// values onto the handoff state machine. claimed/working are accepted as
// delivered so a later richer store can round-trip without a second policy.
func CanonicalMessageStatus(status string) HandoffStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "claimed", "working":
		return HandoffDelivered
	default:
		return NormalizeHandoffStatus(status)
	}
}

func PersistableMessageStatus(status HandoffStatus) string {
	switch CanonicalMessageStatus(string(status)) {
	case HandoffQueued:
		return "queued"
	case HandoffDelivered, HandoffClaimed, HandoffWorking:
		return "delivered"
	case HandoffFailed:
		return "failed"
	case HandoffDeclined:
		return "canceled"
	default:
		return "consumed"
	}
}

func ValidMessageTransition(from, to string) bool {
	return ValidHandoffTransition(string(CanonicalMessageStatus(from)), string(CanonicalMessageStatus(to)))
}
