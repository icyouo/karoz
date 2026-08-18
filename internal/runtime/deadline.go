package runtime

import (
	"context"
	"time"
)

// Deadline is the shared execution-lifetime policy for an owned operation.
// Nil selects a durable default; zero explicitly means unlimited.
type Deadline struct {
	Duration  time.Duration
	Unlimited bool
}

func DeadlineFromMilliseconds(value *int64, fallback time.Duration) Deadline {
	if value == nil {
		return Deadline{Duration: fallback}
	}
	if *value == 0 {
		return Deadline{Unlimited: true}
	}
	return Deadline{Duration: time.Duration(*value) * time.Millisecond}
}

func (deadline Deadline) Bind(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if deadline.Unlimited {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, deadline.Duration)
}
