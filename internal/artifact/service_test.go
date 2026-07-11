package artifact

import (
	"testing"
	"time"
)

type memoryRepository struct{ artifacts map[string]Artifact }

func (repository *memoryRepository) FindActiveByPath(projectID, agentID, path string) (Artifact, bool, error) {
	for _, artifact := range repository.artifacts {
		if artifact.ProjectID == projectID && artifact.AgentID == agentID && artifact.Path == path && artifact.Status != string(Superseded) {
			return artifact, true, nil
		}
	}
	return Artifact{}, false, nil
}

func (repository *memoryRepository) Get(projectID, artifactID string) (Artifact, bool, error) {
	artifact, ok := repository.artifacts[artifactID]
	return artifact, ok && artifact.ProjectID == projectID, nil
}

func (repository *memoryRepository) Save(artifact Artifact) error {
	repository.artifacts[artifact.ID] = artifact
	return nil
}

type memoryEvents struct{ changes []Change }

func (events *memoryEvents) ArtifactChanged(change Change) {
	events.changes = append(events.changes, change)
}

func TestArtifactServiceRevisionAndReview(t *testing.T) {
	repository := &memoryRepository{artifacts: map[string]Artifact{}}
	events := &memoryEvents{}
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	service := Service{Repository: repository, Events: events, NewID: func() string { return "artifact-1" }, Now: func() time.Time { return now }}
	artifact, err := service.RegisterRevision(RegisterRevisionInput{
		ProjectID: "p1", AgentID: "designer", RunID: "run-1", Path: "checkout.html", MimeType: "text/html",
		Kind: "mockup_html", KindProvided: true, Title: "Checkout", TitleProvided: true,
		ContentSHA256: "hash-1", SizeBytes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ID != "artifact-1" || artifact.Revision != 1 || artifact.Status != string(Draft) || len(events.changes) != 1 {
		t.Fatalf("created artifact = %+v changes=%+v", artifact, events.changes)
	}
	if _, err := service.Transition("p1", artifact.ID, "designer", string(Reviewing), "ready"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Transition("p1", artifact.ID, "designer", string(Approved), "self"); err == nil {
		t.Fatal("author approved their own artifact")
	}
	approved, err := service.Transition("p1", artifact.ID, "reviewer", string(Approved), "good")
	if err != nil {
		t.Fatal(err)
	}
	if approved.ApprovedBy != "reviewer" || approved.ApprovedAt == nil {
		t.Fatalf("approved artifact = %+v", approved)
	}
	now = now.Add(time.Hour)
	revised, err := service.RegisterRevision(RegisterRevisionInput{
		ProjectID: "p1", AgentID: "designer", RunID: "run-2", Path: "checkout.html", MimeType: "text/html",
		Kind: "mockup_html", ContentSHA256: "hash-2", SizeBytes: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if revised.Revision != 2 || revised.Status != string(Draft) || revised.ApprovedAt != nil || revised.Title != "Checkout" || len(revised.Revisions) != 2 {
		t.Fatalf("revised artifact = %+v", revised)
	}
}
