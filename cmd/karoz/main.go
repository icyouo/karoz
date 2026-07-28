package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "process-guard" {
		os.Exit(runBackgroundProcessGuard(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "mcp-bridge" {
		if err := runClaudeMCPBridge(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			log.Printf("mcp bridge: %v", err)
			os.Exit(1)
		}
		return
	}
	addr := getenv("KAROZ_ADDR", "127.0.0.1:8088")
	allowNonLoopbackListen := envFlagEnabled("KAROZ_ALLOW_NON_LOOPBACK_LISTEN")
	// Validate before opening persistence files, recovering tasks, or making
	// project directories. A rejected network binding must be a true failed
	// startup, not a partially initialized Studio.
	if err := validateStudioListenAddr(addr, allowNonLoopbackListen); err != nil {
		log.Fatalf("invalid KAROZ_ADDR: %v", err)
	}
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
	a.resumeActionablePlans()

	log.Printf("karoz listening on %s projects_root=%s data_dir=%s", addr, a.settings.ProjectsRoot, a.settings.DataDir)
	if err := http.ListenAndServe(addr, withLogging(withRecovery(a.httpHandler()))); err != nil {
		log.Fatal(err)
	}
}

func envFlagEnabled(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	return value == "1" || strings.EqualFold(value, "true")
}

// validateStudioListenAddr keeps a host-run unauthenticated Studio private to
// the local machine. Containers must listen on their network interface for a
// loopback-only published port to reach them, so Compose explicitly permits
// only wildcard addresses; concrete non-loopback addresses stay rejected even
// when the exception is enabled.
func validateStudioListenAddr(addr string, allowNonLoopbackListen bool) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	if allowNonLoopbackListen {
		if host == "" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			return nil
		}
	}
	if host == "" || (net.ParseIP(host) != nil && net.ParseIP(host).IsUnspecified()) {
		return fmt.Errorf("%q is a wildcard address; set KAROZ_ALLOW_NON_LOOPBACK_LISTEN=1 only with an explicitly loopback-published container port", addr)
	}
	return fmt.Errorf("%q is not localhost or a loopback IP", addr)
}
