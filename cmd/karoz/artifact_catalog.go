package main

import "sync"

// artifactCatalog owns the durable per-project Artifact projection. The
// artifact domain service supplies revision/status policy through this store.
type artifactCatalog struct {
	artifacts map[string][]Artifact
	opsMu     sync.Mutex
}

func newArtifactCatalog() *artifactCatalog {
	return &artifactCatalog{artifacts: map[string][]Artifact{}}
}

func (a *app) artifactCatalogLocked() *artifactCatalog {
	if a.artifactCatalog == nil {
		a.artifactCatalog = newArtifactCatalog()
	}
	return a.artifactCatalog
}
