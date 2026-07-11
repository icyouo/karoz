package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	artifactdomain "github.com/karoz/karoz/internal/artifact"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ArtifactDraft      = string(artifactdomain.Draft)
	ArtifactReviewing  = string(artifactdomain.Reviewing)
	ArtifactApproved   = string(artifactdomain.Approved)
	ArtifactSuperseded = string(artifactdomain.Superseded)
)

func validArtifactKind(kind string) bool {
	return artifactdomain.ValidKind(kind)
}

func inferArtifactKind(path string) string {
	return artifactdomain.InferKind(path)
}

func artifactPreviewable(mimeType string) bool {
	return artifactdomain.Previewable(mimeType)
}

func artifactContentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func (a *app) registerWorkspaceArtifact(projectID, agentID, runID, relPath, kind, title, description string, content []byte) (Artifact, error) {
	a.artifactOpsMu.Lock()
	defer a.artifactOpsMu.Unlock()
	return a.artifactService().RegisterRevision(workspaceArtifactRegistrationInput(projectID, agentID, runID, relPath, kind, title, description, content))
}

func workspaceArtifactRegistrationInput(projectID, agentID, runID, relPath, kind, title, description string, content []byte) artifactdomain.RegisterRevisionInput {
	relPath = filepath.ToSlash(filepath.Clean(strings.TrimSpace(relPath)))
	kind = strings.ToLower(strings.TrimSpace(kind))
	kindProvided := kind != ""
	title = strings.TrimSpace(title)
	titleProvided := title != ""
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(relPath), filepath.Ext(relPath))
	}
	description = strings.TrimSpace(description)
	descriptionProvided := description != ""
	mimeType := mimeTypeForPath(relPath)
	return artifactdomain.RegisterRevisionInput{
		ProjectID: projectID, AgentID: agentID, RunID: runID, Path: relPath, MimeType: mimeType,
		Kind: kind, KindProvided: kindProvided, Title: title, TitleProvided: titleProvided,
		Description: description, DescriptionProvided: descriptionProvided,
		ContentSHA256: artifactContentHash(content), SizeBytes: int64(len(content)),
	}
}

func (a *app) artifactsForProject(projectID, agentID, kind, status string) []Artifact {
	a.mu.Lock()
	items := append([]Artifact{}, a.artifacts[projectID]...)
	a.mu.Unlock()
	agentID = strings.TrimSpace(agentID)
	kind = strings.ToLower(strings.TrimSpace(kind))
	status = strings.ToLower(strings.TrimSpace(status))
	out := make([]Artifact, 0, len(items))
	for _, artifact := range items {
		if agentID != "" && artifact.AgentID != agentID || kind != "" && artifact.Kind != kind || status != "" && artifact.Status != status {
			continue
		}
		out = append(out, artifact)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func (a *app) artifactByID(projectID, artifactID string) (Artifact, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, artifact := range a.artifacts[projectID] {
		if artifact.ID == artifactID {
			return artifact, true
		}
	}
	return Artifact{}, false
}

func (a *app) validateArtifactRefs(projectID string, artifactIDs []string) ([]Artifact, error) {
	seen := map[string]bool{}
	out := make([]Artifact, 0, len(artifactIDs))
	for _, id := range artifactIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		artifact, ok := a.artifactByID(projectID, id)
		if !ok || artifact.Status == ArtifactSuperseded {
			return nil, fmt.Errorf("artifact %s not found or superseded", id)
		}
		seen[id] = true
		out = append(out, artifact)
	}
	return out, nil
}

func validArtifactStatusTransition(from, to string) bool {
	return artifactdomain.ValidStatusTransition(from, to)
}

func (a *app) updateArtifactStatus(projectID, artifactID, actorID, next, note string) (Artifact, error) {
	a.artifactOpsMu.Lock()
	defer a.artifactOpsMu.Unlock()
	return a.artifactService().Transition(projectID, artifactID, actorID, next, note)
}

func (a *app) artifactPreview(projectID, artifactID string) (WorkspaceFilePreview, error) {
	artifact, ok := a.artifactByID(projectID, artifactID)
	if !ok {
		return WorkspaceFilePreview{}, errors.New("artifact not found")
	}
	if !artifact.Previewable {
		return WorkspaceFilePreview{}, errors.New("artifact is not previewable")
	}
	return a.getWorkspaceFilePreview(projectID, artifact.AgentID, artifact.Path)
}

func artifactRequiresApprovalForBuild(artifact Artifact) bool {
	return artifactdomain.RequiresApprovalForBuild(artifact.Kind)
}

func (a *app) validateTaskArtifactRefs(projectID string, artifactIDs []string) ([]string, error) {
	artifacts, err := a.validateArtifactRefs(projectID, artifactIDs)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		if artifactRequiresApprovalForBuild(artifact) && artifact.Status != ArtifactApproved {
			return nil, fmt.Errorf("design artifact %s must be approved before creating an implementation task", artifact.ID)
		}
		ids = append(ids, artifact.ID)
	}
	return ids, nil
}

func (a *app) reconcileWorkspaceArtifacts() error {
	a.mu.Lock()
	if a.artifacts == nil {
		a.artifacts = map[string][]Artifact{}
	}
	agents := map[string][]Agent{}
	for projectID, projectAgents := range a.agents {
		agents[projectID] = append([]Agent{}, projectAgents...)
	}
	a.mu.Unlock()
	changed := false
	for projectID, projectAgents := range agents {
		for _, agent := range projectAgents {
			files, err := a.listWorkspaceFiles(projectID, agent.ID)
			if err != nil {
				return err
			}
			for _, file := range files {
				found := false
				for _, artifact := range a.artifactsForProject(projectID, agent.ID, "", "") {
					if artifact.Path == file.Path && artifact.Status != ArtifactSuperseded {
						found = true
						break
					}
				}
				if found {
					continue
				}
				full, err := a.safeWorkspacePath(projectID, agent.ID, file.Path)
				if err != nil {
					return err
				}
				content, err := os.ReadFile(full)
				if err != nil {
					return err
				}
				now := file.UpdatedAt.UTC()
				if now.IsZero() {
					now = time.Now().UTC()
				}
				revision := ArtifactRevision{Revision: 1, Status: ArtifactDraft, ContentSHA256: artifactContentHash(content), SizeBytes: file.SizeBytes, CreatedByAgentID: agent.ID, CreatedAt: now}
				artifact := Artifact{
					ID: randomID(), ProjectID: projectID, AgentID: agent.ID, Kind: inferArtifactKind(file.Path),
					Title: strings.TrimSuffix(file.Filename, filepath.Ext(file.Filename)), Path: file.Path, MimeType: file.MimeType,
					Revision: 1, Status: ArtifactDraft, Previewable: artifactPreviewable(file.MimeType), Revisions: []ArtifactRevision{revision}, CreatedAt: now, UpdatedAt: now,
				}
				a.mu.Lock()
				a.artifacts[projectID] = append(a.artifacts[projectID], artifact)
				a.mu.Unlock()
				changed = true
			}
		}
	}
	if changed {
		return a.saveArtifacts()
	}
	return nil
}
