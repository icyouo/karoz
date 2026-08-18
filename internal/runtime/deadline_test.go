package runtime

import (
	"context"
	"testing"
	"time"
)

func TestDeadlinePolicySupportsDefaultFiniteAndUnlimited(t *testing.T) {
	zero, finite := int64(0), int64(25)
	if got := DeadlineFromMilliseconds(nil, time.Second); got.Unlimited || got.Duration != time.Second {
		t.Fatalf("default deadline = %+v", got)
	}
	if got := DeadlineFromMilliseconds(&finite, time.Second); got.Unlimited || got.Duration != 25*time.Millisecond {
		t.Fatalf("finite deadline = %+v", got)
	}
	ctx, cancel := DeadlineFromMilliseconds(&zero, time.Second).Bind(context.Background())
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("unlimited deadline installed a timeout")
	}
}
