package main

import (
	persistenceadapter "github.com/karoz/karoz/internal/persistence"
	"os"
)

func (a *app) loadJSON(name string, target any) (bool, error) {
	return persistenceadapter.NewJSONStore(a.settings.DataDir).Load(name, target)
}

func (a *app) saveJSON(name string, value any, perm os.FileMode) error {
	return persistenceadapter.NewJSONStore(a.settings.DataDir).Save(name, value, perm)
}
