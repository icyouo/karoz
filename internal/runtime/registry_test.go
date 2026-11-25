package runtime

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRegistryRejectsSecondActiveRunForSameOwner(t *testing.T) {
	registry := NewRegistry()
	first, started := registry.Lifecycle().Begin(RunInput{ProjectID: "p1", AgentID: "agent-1"}, "run-1")
	if !started || first.ID != "run-1" {
		t.Fatalf("first begin = %+v started=%v", first, started)
	}
	duplicate, started := registry.Lifecycle().Begin(RunInput{ProjectID: "p1", AgentID: "agent-1"}, "run-2")
	if started || duplicate.ID != "run-1" {
		t.Fatalf("duplicate begin = %+v started=%v", duplicate, started)
	}
	if _, _, _, ok := registry.Lookup("run-1"); !ok {
		t.Fatal("active run was not indexed by id")
	}
}

func TestRegistryCancelBlocksConcurrentResultCommit(t *testing.T) {
	registry := NewRegistry()
	if _, started := registry.Lifecycle().Begin(RunInput{ProjectID: "p1", AgentID: "agent-1"}, "run-1"); !started {
		t.Fatal("begin failed")
	}
	ctx, claimed := registry.Control().ClaimAndBindWorkerContext(context.Background(), "p1", "agent-1", "run-1")
	if !claimed {
		t.Fatal("worker claim failed")
	}

	var wg sync.WaitGroup
	var cancelAccepted, commitAccepted bool
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		request, ok := registry.Control().RequestCancel("p1", "agent-1")
		cancelAccepted = ok
		if ok && request.Cancel != nil {
			request.Cancel()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		_, _, commitAccepted = registry.Control().CommitResult("p1", "agent-1", "run-1", func(run Run) (Run, bool) {
			return Finish(run, StateDone, nil, time.Now().UTC()), true
		})
	}()
	close(start)
	wg.Wait()

	if cancelAccepted == commitAccepted {
		t.Fatalf("cancel and commit must be mutually exclusive, cancel=%v commit=%v", cancelAccepted, commitAccepted)
	}
	if cancelAccepted {
		if ctx.Err() != context.Canceled {
			t.Fatalf("worker context error = %v", ctx.Err())
		}
		_, _, _, ok := registry.Control().FinishRun("p1", "agent-1", "run-1", StateCancelled, context.Canceled, time.Now().UTC())
		if !ok {
			t.Fatal("cancelled run could not finish")
		}
	}
	if _, active := registry.Lifecycle().Active("p1", "agent-1"); active {
		t.Fatal("run remained active after the winning terminal claim")
	}
}

func TestRegistryCommitRemovesRunSoCancelMisses(t *testing.T) {
	registry := NewRegistry()
	if _, started := registry.Lifecycle().Begin(RunInput{ProjectID: "p1", AgentID: "agent-1"}, "run-1"); !started {
		t.Fatal("begin failed")
	}
	if _, claimed := registry.Control().ClaimAndBindWorkerContext(context.Background(), "p1", "agent-1", "run-1"); !claimed {
		t.Fatal("worker claim failed")
	}
	_, _, committed := registry.Control().CommitResult("p1", "agent-1", "run-1", func(run Run) (Run, bool) {
		return Finish(run, StateDone, nil, time.Now().UTC()), true
	})
	if !committed {
		t.Fatal("result commit failed")
	}
	if _, accepted := registry.Control().RequestCancel("p1", "agent-1"); accepted {
		t.Fatal("cancel accepted after successful commit")
	}
	if _, started := registry.Lifecycle().Begin(RunInput{ProjectID: "p1", AgentID: "agent-1"}, "run-2"); !started {
		t.Fatal("owner remained busy after successful commit")
	}
}

func TestRegistryWaitInactiveWakesAfterFinish(t *testing.T) {
	registry := NewRegistry()
	if _, started := registry.Lifecycle().Begin(RunInput{ProjectID: "p1", AgentID: "agent-1"}, "run-1"); !started {
		t.Fatal("begin failed")
	}
	done := make(chan bool, 1)
	go func() {
		done <- registry.WaitInactive(context.Background(), "p1", "agent-1")
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("wait returned while run was active")
	default:
	}
	if _, _, ok := registry.Lifecycle().Finish("p1", "agent-1", "run-1", StateDone, nil); !ok {
		t.Fatal("finish failed")
	}
	select {
	case idle := <-done:
		if !idle {
			t.Fatal("wait returned false after finish")
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not observe finish")
	}
}

func TestRegistryPersistenceDoesNotBlockOtherOwners(t *testing.T) {
	r := NewRegistry()
	r.Lifecycle().Begin(RunInput{ProjectID: "p", AgentID: "a"}, "first")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Control().CommitResult("p", "a", "first", func(run Run) (Run, bool) {
			close(entered)
			<-release
			return Finish(run, StateDone, nil, time.Now()), true
		})
	}()
	<-entered
	sibling := make(chan bool, 1)
	go func() { _, ok := r.Lifecycle().Begin(RunInput{ProjectID: "p", AgentID: "b"}, "second"); sibling <- ok }()
	select {
	case ok := <-sibling:
		if !ok {
			t.Fatal("sibling run blocked")
		}
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("database callback held global registry lock")
	}
	if _, _, _, ok := r.Lookup("first"); !ok {
		t.Fatal("owner released before commit")
	}
	close(release)
	<-done
}
