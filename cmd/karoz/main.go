package main

import (
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	projectsRootFromEnv := strings.TrimSpace(os.Getenv("KAROZ_PROJECTS_ROOT")) != ""
	settings := Settings{
		DataDir:      getenv("KAROZ_DATA_DIR", ".karoz"),
		ProjectsRoot: defaultProjectsRoot(),
	}
	if projectsRootFromEnv {
		value := strings.TrimSpace(os.Getenv("KAROZ_PROJECTS_ROOT"))
		settings.ProjectsRoot = value
	}
	settings.DataDir = expandHome(settings.DataDir)
	settings.ProjectsRoot = expandHome(settings.ProjectsRoot)
	settings.ExtraProjectsRoots = normalizeWorkspaceRoots(settings.ExtraProjectsRoots, settings.ProjectsRoot)

	a := newApp(settings)
	if !projectsRootFromEnv {
		if err := a.loadSettings(); err != nil {
			log.Fatalf("load settings: %v", err)
		}
	}
	if err := a.bootstrap(); err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	if err := os.MkdirAll(a.settings.ProjectsRoot, 0755); err != nil {
		log.Fatalf("create projects root: %v", err)
	}
	for _, root := range a.settings.ExtraProjectsRoots {
		if err := os.MkdirAll(root, 0755); err != nil {
			log.Fatalf("create extra projects root: %v", err)
		}
	}
	if err := a.recoverInterruptedTasks(); err != nil {
		log.Printf("recover interrupted tasks: %v", err)
	}
	a.resumeScheduledRuns()

	addr := getenv("KAROZ_ADDR", "127.0.0.1:8088")
	log.Printf("karoz listening on %s projects_root=%s data_dir=%s", addr, a.settings.ProjectsRoot, a.settings.DataDir)
	if err := http.ListenAndServe(addr, withLogging(a.httpHandler())); err != nil {
		log.Fatal(err)
	}
}
