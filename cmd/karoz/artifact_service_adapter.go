package main

import (
	artifactdomain "github.com/karoz/karoz/internal/artifact"
)

type appArtifactRepository struct{ app *app }

func (repository appArtifactRepository) FindActiveByPath(projectID, agentID, path string) (Artifact, bool, error) {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	for _, artifact := range repository.app.artifacts[projectID] {
		if artifact.AgentID == agentID && artifact.Path == path && artifact.Status != ArtifactSuperseded {
			return artifact, true, nil
		}
	}
	return Artifact{}, false, nil
}

func (repository appArtifactRepository) Get(projectID, artifactID string) (Artifact, bool, error) {
	artifact, ok := repository.app.artifactByID(projectID, artifactID)
	return artifact, ok, nil
}

func (repository appArtifactRepository) Save(artifact Artifact) error {
	repository.app.mu.Lock()
	if repository.app.artifacts == nil {
		repository.app.artifacts = map[string][]Artifact{}
	}
	items := repository.app.artifacts[artifact.ProjectID]
	found := false
	for i := range items {
		if items[i].ID == artifact.ID {
			items[i] = artifact
			found = true
			break
		}
	}
	if !found {
		items = append(items, artifact)
	}
	repository.app.artifacts[artifact.ProjectID] = items
	repository.app.mu.Unlock()
	return repository.app.saveArtifacts()
}

type appArtifactEvents struct{ app *app }

func (events appArtifactEvents) ArtifactChanged(change artifactdomain.Change) {
	events.app.emitRuntimeStateChanged(RuntimeEvent{
		ID: randomID(), ProjectID: change.Artifact.ProjectID, Kind: "artifact_changed", EntityID: change.Artifact.ID,
		RunID: change.RunID, From: change.From, To: change.To, Reason: change.Reason, CreatedAt: change.At,
	})
}

func (a *app) artifactService() artifactdomain.Service {
	return artifactdomain.Service{
		Repository: appArtifactRepository{app: a},
		Events:     appArtifactEvents{app: a},
		NewID:      randomID,
	}
}
