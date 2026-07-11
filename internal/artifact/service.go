package artifact

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type Repository interface {
	FindActiveByPath(projectID, agentID, path string) (Artifact, bool, error)
	Get(projectID, artifactID string) (Artifact, bool, error)
	Save(Artifact) error
}

type Change struct {
	Artifact Artifact
	From     string
	To       string
	Reason   string
	RunID    string
	At       time.Time
}

type EventSink interface {
	ArtifactChanged(Change)
}

type Service struct {
	Repository Repository
	Events     EventSink
	NewID      func() string
	Now        func() time.Time
}

type RegisterRevisionInput struct {
	ProjectID           string
	AgentID             string
	RunID               string
	Path                string
	MimeType            string
	Kind                string
	KindProvided        bool
	Title               string
	TitleProvided       bool
	Description         string
	DescriptionProvided bool
	ContentSHA256       string
	SizeBytes           int64
}

func ValidateRegisterRevisionInput(input RegisterRevisionInput) error {
	projectID := strings.TrimSpace(input.ProjectID)
	agentID := strings.TrimSpace(input.AgentID)
	path := strings.TrimSpace(input.Path)
	kind := strings.ToLower(strings.TrimSpace(input.Kind))
	if kind == "" {
		kind = InferKind(path)
	}
	if projectID == "" || agentID == "" || path == "" {
		return errors.New("project id, agent id, and path are required")
	}
	if !ValidKind(kind) {
		return fmt.Errorf("invalid artifact kind %q", kind)
	}
	return nil
}

func (service Service) RegisterRevision(input RegisterRevisionInput) (Artifact, error) {
	if service.Repository == nil || service.NewID == nil {
		return Artifact{}, errors.New("artifact service is not configured")
	}
	now := service.now()
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.AgentID = strings.TrimSpace(input.AgentID)
	input.Path = strings.TrimSpace(input.Path)
	input.Kind = strings.ToLower(strings.TrimSpace(input.Kind))
	if input.Kind == "" {
		input.Kind = InferKind(input.Path)
	}
	if err := ValidateRegisterRevisionInput(input); err != nil {
		return Artifact{}, err
	}
	revision := Revision{
		Status: string(Draft), Description: strings.TrimSpace(input.Description), ContentSHA256: strings.TrimSpace(input.ContentSHA256),
		SizeBytes: input.SizeBytes, CreatedByAgentID: input.AgentID, CreatedByRunID: strings.TrimSpace(input.RunID), CreatedAt: now,
	}
	artifact, found, err := service.Repository.FindActiveByPath(input.ProjectID, input.AgentID, input.Path)
	if err != nil {
		return Artifact{}, err
	}
	previous := ""
	if found {
		previous = artifact.Status
		artifact.Revision++
		if input.KindProvided {
			artifact.Kind = input.Kind
		}
		if input.TitleProvided {
			artifact.Title = strings.TrimSpace(input.Title)
		}
		if input.DescriptionProvided {
			artifact.Description = strings.TrimSpace(input.Description)
		}
		artifact.MimeType = input.MimeType
		artifact.Status = string(Draft)
		artifact.Previewable = Previewable(input.MimeType)
		artifact.CreatedByRunID = strings.TrimSpace(input.RunID)
		artifact.ApprovedBy = ""
		artifact.ApprovedAt = nil
		artifact.ReviewNote = ""
		artifact.UpdatedAt = now
		revision.Revision = artifact.Revision
		revision.Description = artifact.Description
		artifact.Revisions = append(artifact.Revisions, revision)
	} else {
		title := strings.TrimSpace(input.Title)
		if title == "" {
			title = input.Path
		}
		revision.Revision = 1
		artifact = Artifact{
			ID: service.NewID(), ProjectID: input.ProjectID, AgentID: input.AgentID, Kind: input.Kind, Title: title,
			Path: input.Path, MimeType: input.MimeType, Revision: 1, Status: string(Draft),
			Description: strings.TrimSpace(input.Description), Previewable: Previewable(input.MimeType),
			CreatedByRunID: strings.TrimSpace(input.RunID), Revisions: []Revision{revision}, CreatedAt: now, UpdatedAt: now,
		}
	}
	if err := service.Repository.Save(artifact); err != nil {
		return Artifact{}, err
	}
	service.publish(Change{Artifact: artifact, From: previous, To: artifact.Status, Reason: "artifact_revision_written", RunID: input.RunID, At: now})
	return artifact, nil
}

func (service Service) Transition(projectID, artifactID, actorID, next, note string) (Artifact, error) {
	if service.Repository == nil {
		return Artifact{}, errors.New("artifact service is not configured")
	}
	next = strings.ToLower(strings.TrimSpace(next))
	if next != string(Draft) && next != string(Reviewing) && next != string(Approved) && next != string(Superseded) {
		return Artifact{}, fmt.Errorf("invalid artifact status %q", next)
	}
	artifact, found, err := service.Repository.Get(projectID, artifactID)
	if err != nil {
		return Artifact{}, err
	}
	if !found {
		return Artifact{}, errors.New("artifact not found")
	}
	if !ValidStatusTransition(artifact.Status, next) {
		return Artifact{}, fmt.Errorf("invalid artifact transition %s -> %s", artifact.Status, next)
	}
	actorID = strings.TrimSpace(actorID)
	if next == string(Approved) && actorID == artifact.AgentID && actorID != "karoz" && actorID != "user" {
		return Artifact{}, errors.New("artifact author cannot approve their own artifact")
	}
	previous := artifact.Status
	now := service.now()
	artifact.Status = next
	artifact.ReviewNote = strings.TrimSpace(note)
	artifact.UpdatedAt = now
	if next == string(Approved) {
		if actorID == "" {
			actorID = "user"
		}
		artifact.ApprovedBy = actorID
		artifact.ApprovedAt = &now
	} else if previous == string(Approved) || next == string(Draft) {
		artifact.ApprovedBy = ""
		artifact.ApprovedAt = nil
	}
	if len(artifact.Revisions) > 0 {
		artifact.Revisions[len(artifact.Revisions)-1].Status = next
	}
	if err := service.Repository.Save(artifact); err != nil {
		return Artifact{}, err
	}
	service.publish(Change{Artifact: artifact, From: previous, To: next, Reason: "artifact_status_changed", At: now})
	return artifact, nil
}

func (service Service) now() time.Time {
	if service.Now != nil {
		return service.Now().UTC()
	}
	return time.Now().UTC()
}

func (service Service) publish(change Change) {
	if service.Events != nil {
		service.Events.ArtifactChanged(change)
	}
}
