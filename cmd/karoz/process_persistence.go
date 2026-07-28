package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
	processdomain "github.com/karoz/karoz/internal/process"
)

const (
	processSnapshotSchemaVersion = 1
	runtimeIndexSchemaVersion    = 1
	runtimeMutationSchemaVersion = 1
	processAuthorityID           = "processes"
	processTerminalEventKind     = "process_changed"
)

type processPersistenceFailpoint string

const (
	processPersistAfterIndexInitializing   processPersistenceFailpoint = "after_index_initializing"
	processPersistAfterLedgerInitialize    processPersistenceFailpoint = "after_ledger_initialize"
	processPersistAfterJournalInitialize   processPersistenceFailpoint = "after_journal_initialize"
	processPersistAfterAuthorityInitialize processPersistenceFailpoint = "after_authority_initialize"
	processPersistAfterIndexReady          processPersistenceFailpoint = "after_index_ready"
	processPersistAfterIntent              processPersistenceFailpoint = "after_intent"
	processPersistAfterTokenSelection      processPersistenceFailpoint = "after_token_selection"
	processPersistAfterLedgerAllocate      processPersistenceFailpoint = "after_ledger_allocate"
	processPersistAfterAllocatedOperation  processPersistenceFailpoint = "after_allocated_operation"
	processPersistAfterAuthorityCreate     processPersistenceFailpoint = "after_authority_create"
	processPersistAfterAuthorityOperation  processPersistenceFailpoint = "after_authority_operation"
	processPersistAfterLedgerActivate      processPersistenceFailpoint = "after_ledger_activate"
	processPersistAfterActiveOperation     processPersistenceFailpoint = "after_active_operation"
	processPersistAfterAuthorityActive     processPersistenceFailpoint = "after_authority_active"
	processPersistAfterCommit              processPersistenceFailpoint = "after_commit"
	processPersistAfterTerminal            processPersistenceFailpoint = "after_terminal"
	processPersistAfterLedgerTerminal      processPersistenceFailpoint = "after_ledger_terminal"
	processPersistAfterReleaseIntent       processPersistenceFailpoint = "after_release_intent"
	processPersistAfterAck                 processPersistenceFailpoint = "after_terminal_ack"
	processPersistAfterReleaseOperation    processPersistenceFailpoint = "after_release_operation"
	processPersistAfterReleaseMark         processPersistenceFailpoint = "after_release_mark"
	processPersistAfterAuthorityDetach     processPersistenceFailpoint = "after_authority_detach"
	processPersistAfterDetachedOperation   processPersistenceFailpoint = "after_detached_operation"
	processPersistAfterLedgerRelease       processPersistenceFailpoint = "after_ledger_release"
	processPersistAfterAdmissionDelete     processPersistenceFailpoint = "after_admission_delete"
)

type runtimeProjectIndex struct {
	SchemaVersion int                                 `json:"schema_version"`
	Projects      map[string]runtimeProjectIndexEntry `json:"projects"`
}

type runtimeProjectIndexEntry struct {
	Project    monitordomain.RuntimeProjectIdentity `json:"project"`
	State      string                               `json:"state"`
	Generation uint64                               `json:"generation"`
}

type processAuthoritySnapshot struct {
	SchemaVersion int                                `json:"schema_version"`
	Generation    uint64                             `json:"generation"`
	Projects      map[string]processAuthorityProject `json:"projects"`
}

type processAuthorityProject struct {
	Project    monitordomain.RuntimeProjectIdentity `json:"project"`
	Records    map[string]durableProcessRecord      `json:"records"`
	Tombstones map[string]processTombstone          `json:"tombstones"`
}

type durableProcessRecord struct {
	Process             processdomain.Process              `json:"process"`
	Reservation         *monitordomain.TerminalReservation `json:"reservation,omitempty"`
	Event               *processTerminalEvent              `json:"terminal_event,omitempty"`
	AcknowledgedEventID string                             `json:"acknowledged_event_id,omitempty"`
}

type processTerminalEvent struct {
	ID          string                               `json:"id"`
	Kind        string                               `json:"kind"`
	Project     monitordomain.RuntimeProjectIdentity `json:"project"`
	ProcessID   string                               `json:"process_id"`
	State       processdomain.State                  `json:"state"`
	ExitCode    int                                  `json:"exit_code"`
	OccurredAt  time.Time                            `json:"occurred_at"`
	Reservation monitordomain.TerminalReservation    `json:"reservation"`
}

type processTombstone struct {
	ProcessID string    `json:"process_id"`
	LogPath   string    `json:"log_path"`
	CreatedAt time.Time `json:"created_at"`
}

type runtimeMutationSnapshot struct {
	SchemaVersion int                                               `json:"schema_version"`
	Project       monitordomain.RuntimeProjectIdentity              `json:"project"`
	Generation    uint64                                            `json:"generation"`
	Operations    map[string]monitordomain.RuntimeMutationOperation `json:"operations"`
}

type processProjectRuntime struct {
	identity  monitordomain.RuntimeProjectIdentity
	lane      sync.Mutex
	ledgerMu  sync.Mutex
	journalMu sync.Mutex
	ledger    monitordomain.TerminalReservationLedger
	journal   runtimeMutationSnapshot
}

type processRuntimePersistence struct {
	store       *secureRuntimeStore
	authorityMu sync.Mutex
	authority   processAuthoritySnapshot
	projects    map[string]*processProjectRuntime
	fail        func(processPersistenceFailpoint) error
	now         func() time.Time
	retention   processdomain.RetentionPolicy
}

func newProcessRuntimePersistence(
	dataDir string,
	projects []Project,
	fail func(processPersistenceFailpoint) error,
) (*processRuntimePersistence, error) {
	store, err := newSecureRuntimeStore(dataDir)
	if err != nil {
		return nil, err
	}
	identities, err := resolveRuntimeProjectIdentities(dataDir, projects)
	if err != nil {
		return nil, err
	}
	runtime := &processRuntimePersistence{
		store: store, projects: make(map[string]*processProjectRuntime),
		fail: fail, now: func() time.Time { return time.Now().UTC() },
		retention: defaultProcessRetentionPolicy(),
	}
	if err := runtime.bootstrap(identities); err != nil {
		return nil, err
	}
	return runtime, nil
}

func resolveRuntimeProjectIdentities(
	dataDir string,
	projects []Project,
) ([]monitordomain.RuntimeProjectIdentity, error) {
	dataRoot, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(dataRoot); resolveErr == nil {
		dataRoot = resolved
	} else if parent, parentErr := filepath.EvalSymlinks(filepath.Dir(dataRoot)); parentErr == nil {
		dataRoot = filepath.Join(parent, filepath.Base(dataRoot))
	}
	runtimeRoot := filepath.Join(filepath.Clean(dataRoot), "project-runtime")
	seenPaths := make(map[string]string, len(projects))
	seenIDs := make(map[string]bool, len(projects))
	result := make([]monitordomain.RuntimeProjectIdentity, 0, len(projects))
	for _, project := range projects {
		if strings.TrimSpace(project.ID) == "" || strings.TrimSpace(project.Path) == "" {
			return nil, errors.New("project identity is incomplete")
		}
		if seenIDs[project.ID] {
			return nil, errors.New("duplicate canonical project id")
		}
		seenIDs[project.ID] = true
		canonical, err := filepath.EvalSymlinks(project.Path)
		if err != nil {
			return nil, fmt.Errorf("canonicalize project %s: %w", project.ID, err)
		}
		canonical, err = filepath.Abs(canonical)
		if err != nil {
			return nil, err
		}
		canonical = filepath.Clean(canonical)
		if other := seenPaths[canonical]; other != "" && other != project.ID {
			return nil, fmt.Errorf("projects %s and %s share one canonical path", other, project.ID)
		}
		seenPaths[canonical] = project.ID
		if pathInside(runtimeRoot, canonical) {
			return nil, errors.New("runtime data root is nested inside a project")
		}
		identity := monitordomain.RuntimeProjectIdentity{
			ProjectID: project.ID, CanonicalProjectPath: canonical,
			CanonicalPathSHA256: monitordomain.CanonicalProjectPathSHA256(canonical),
			SafeProjectKey:      monitordomain.SafeProjectKey(project.ID),
		}
		if err := identity.Validate(); err != nil {
			return nil, err
		}
		result = append(result, identity)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SafeProjectKey < result[j].SafeProjectKey })
	return result, nil
}

func pathInside(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (runtime *processRuntimePersistence) bootstrap(
	identities []monitordomain.RuntimeProjectIdentity,
) error {
	if err := runtime.store.ensureDir("project-runtime"); err != nil {
		return err
	}
	if err := runtime.store.ensureDir("process-logs"); err != nil {
		return err
	}
	var index runtimeProjectIndex
	indexFound, err := runtime.store.loadJSON(filepath.Join("project-runtime", "index.json"), &index)
	if err != nil {
		return err
	}
	var authority processAuthoritySnapshot
	authorityFound, err := runtime.store.loadJSON("processes.json", &authority)
	if err != nil {
		return err
	}
	if !indexFound {
		if authorityFound {
			return errors.New("process authority exists without runtime index")
		}
		index = runtimeProjectIndex{SchemaVersion: runtimeIndexSchemaVersion, Projects: map[string]runtimeProjectIndexEntry{}}
	} else {
		if err := validateRuntimeProjectIndex(index); err != nil {
			return err
		}
	}
	if !authorityFound {
		for _, entry := range index.Projects {
			if entry.State != "initializing" {
				return errors.New("established process authority is missing")
			}
		}
		authority = processAuthoritySnapshot{
			SchemaVersion: processSnapshotSchemaVersion,
			Projects:      make(map[string]processAuthorityProject),
		}
	} else {
		if err := validateProcessAuthoritySnapshot(authority); err != nil {
			return err
		}
	}
	identityKeys := make(map[string]bool, len(identities))
	for _, identity := range identities {
		identityKeys[identity.SafeProjectKey] = true
	}
	for key := range index.Projects {
		if !identityKeys[key] {
			return errors.New("runtime index references a missing canonical project")
		}
	}
	for key := range authority.Projects {
		if !identityKeys[key] {
			return errors.New("process authority references a missing canonical project")
		}
	}
	indexNeedsSave := !indexFound
	for _, identity := range identities {
		if _, exists := index.Projects[identity.SafeProjectKey]; !exists {
			index.Projects[identity.SafeProjectKey] = runtimeProjectIndexEntry{
				Project: identity, State: "initializing", Generation: 1,
			}
			indexNeedsSave = true
		}
	}
	if indexNeedsSave {
		if err := runtime.store.saveJSON(filepath.Join("project-runtime", "index.json"), index); err != nil {
			return err
		}
		if err := runtime.persistenceFail(processPersistAfterIndexInitializing); err != nil {
			return err
		}
	}
	for _, identity := range identities {
		entry, exists := index.Projects[identity.SafeProjectKey]
		newProject := !exists
		if exists {
			if entry.Project != identity ||
				(entry.State != "ready" && entry.State != "initializing") ||
				entry.Generation == 0 {
				return errors.New("runtime project index identity/state mismatch")
			}
		}
		projectAuthority, authorityExists := authority.Projects[identity.SafeProjectKey]
		if authorityExists {
			if projectAuthority.Project != identity {
				return errors.New("process authority project identity mismatch")
			}
		} else {
			projectAuthority = processAuthorityProject{
				Project: identity, Records: map[string]durableProcessRecord{},
				Tombstones: map[string]processTombstone{},
			}
			authority.Projects[identity.SafeProjectKey] = projectAuthority
		}
		projectRuntime, err := runtime.loadOrInitializeProject(
			identity, entry.State == "initializing" || !indexFound || newProject,
		)
		if err != nil {
			return err
		}
		runtime.projects[identity.ProjectID] = projectRuntime
	}
	runtime.authority = authority
	if err := runtime.store.saveJSON("processes.json", authority); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterAuthorityInitialize); err != nil {
		return err
	}
	for key, entry := range index.Projects {
		if entry.State == "initializing" {
			entry.State = "ready"
			entry.Generation++
			index.Projects[key] = entry
		}
	}
	if err := runtime.store.saveJSON(filepath.Join("project-runtime", "index.json"), index); err != nil {
		return err
	}
	if err := runtime.persistenceFail(processPersistAfterIndexReady); err != nil {
		return err
	}
	for _, project := range runtime.projects {
		project.lane.Lock()
		err := runtime.recoverProjectLocked(project)
		project.lane.Unlock()
		if err != nil {
			return err
		}
	}
	if err := runtime.recoverInterruptedProcesses(); err != nil {
		return err
	}
	if err := runtime.recoverTombstones(); err != nil {
		return err
	}
	for _, project := range runtime.projects {
		project.lane.Lock()
		err := runtime.applyRetentionLocked(project, runtime.retention, runtime.now())
		project.lane.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (runtime *processRuntimePersistence) loadOrInitializeProject(
	identity monitordomain.RuntimeProjectIdentity,
	allowInitialize bool,
) (*processProjectRuntime, error) {
	dir := filepath.Join("project-runtime", identity.SafeProjectKey)
	if err := runtime.store.ensureDir(dir); err != nil {
		return nil, err
	}
	var ledger monitordomain.TerminalReservationLedger
	ledgerFound, err := runtime.store.loadJSON(filepath.Join(dir, "terminal-reservations.json"), &ledger)
	if err != nil {
		return nil, err
	}
	var journal runtimeMutationSnapshot
	journalFound, err := runtime.store.loadJSON(filepath.Join(dir, "runtime-mutations.json"), &journal)
	if err != nil {
		return nil, err
	}
	if ledgerFound != journalFound && !allowInitialize {
		return nil, errors.New("runtime project ledger/journal establishment mismatch")
	}
	if !ledgerFound && !allowInitialize {
		return nil, errors.New("established runtime project store is missing")
	}
	if !ledgerFound {
		ledger, err = monitordomain.NewTerminalReservationLedger(identity)
		if err != nil {
			return nil, err
		}
	}
	if !journalFound {
		journal = runtimeMutationSnapshot{
			SchemaVersion: runtimeMutationSchemaVersion, Project: identity,
			Operations: map[string]monitordomain.RuntimeMutationOperation{},
		}
	}
	if err := ledger.Validate(identity); err != nil {
		return nil, err
	}
	if err := validateRuntimeMutationSnapshot(journal, identity); err != nil {
		return nil, err
	}
	if ledgerFound != journalFound &&
		(len(ledger.Slots) != 0 || len(journal.Operations) != 0) {
		return nil, errors.New("partial runtime initialization is not pristine")
	}
	if !ledgerFound {
		if err := runtime.store.saveJSON(filepath.Join(dir, "terminal-reservations.json"), ledger); err != nil {
			return nil, err
		}
		if err := runtime.persistenceFail(processPersistAfterLedgerInitialize); err != nil {
			return nil, err
		}
	}
	if !journalFound {
		if err := runtime.store.saveJSON(filepath.Join(dir, "runtime-mutations.json"), journal); err != nil {
			return nil, err
		}
		if err := runtime.persistenceFail(processPersistAfterJournalInitialize); err != nil {
			return nil, err
		}
	}
	return &processProjectRuntime{identity: identity, ledger: ledger, journal: journal}, nil
}

func validateRuntimeProjectIndex(index runtimeProjectIndex) error {
	if index.SchemaVersion != runtimeIndexSchemaVersion || index.Projects == nil {
		return errors.New("invalid runtime project index schema")
	}
	paths := map[string]string{}
	for key, entry := range index.Projects {
		if err := entry.Project.Validate(); err != nil {
			return err
		}
		if key != entry.Project.SafeProjectKey ||
			(entry.State != "ready" && entry.State != "initializing") ||
			entry.Generation == 0 {
			return errors.New("invalid runtime project index entry")
		}
		if other := paths[entry.Project.CanonicalProjectPath]; other != "" && other != entry.Project.ProjectID {
			return errors.New("runtime project index canonical path collision")
		}
		paths[entry.Project.CanonicalProjectPath] = entry.Project.ProjectID
	}
	return nil
}

func validateProcessAuthoritySnapshot(snapshot processAuthoritySnapshot) error {
	if snapshot.SchemaVersion != processSnapshotSchemaVersion || snapshot.Projects == nil {
		return errors.New("invalid process authority schema")
	}
	for key, project := range snapshot.Projects {
		if err := project.Project.Validate(); err != nil {
			return err
		}
		if key != project.Project.SafeProjectKey || project.Records == nil || project.Tombstones == nil {
			return errors.New("invalid process authority project partition")
		}
		for id, record := range project.Records {
			if id == "" || record.Process.ID != id || record.Process.ProjectID != project.Project.ProjectID ||
				!safeProcessID(id) || !record.Process.State.Valid() ||
				record.Process.LogPath != processLogRelativePath(project.Project, id) {
				return errors.New("invalid durable process record")
			}
			if record.Reservation != nil {
				if err := monitordomain.ValidateTerminalReservation(project.Project, *record.Reservation); err != nil {
					return err
				}
				if record.Reservation.AuthorityID != processAuthorityID || record.Reservation.EntityID != id {
					return errors.New("process reservation authority/entity mismatch")
				}
				if !record.Process.State.Terminal() &&
					record.Reservation.State != monitordomain.ReservationAllocating &&
					record.Reservation.State != monitordomain.ReservationActive {
					return errors.New("active process has a terminal reservation state")
				}
			} else if !record.Process.State.Terminal() {
				return errors.New("active process is missing terminal reservation")
			}
			if record.Event != nil {
				if record.AcknowledgedEventID != "" {
					return errors.New("process terminal event is both pending and acknowledged")
				}
				if err := validateProcessTerminalEvent(project.Project, record); err != nil {
					return err
				}
			} else if record.Process.State.Terminal() {
				if record.AcknowledgedEventID != processTerminalEventID(id) {
					return errors.New("terminal process lacks an exact acknowledgement marker")
				}
				if record.Reservation != nil &&
					record.Reservation.State != monitordomain.ReservationTerminalUnacknowledged &&
					record.Reservation.State != monitordomain.ReservationReleasing {
					return errors.New("acknowledged terminal process has invalid reservation state")
				}
			} else if record.AcknowledgedEventID != "" {
				return errors.New("active process has a terminal acknowledgement marker")
			}
		}
		for id, tombstone := range project.Tombstones {
			if !safeProcessID(id) || tombstone.ProcessID != id ||
				tombstone.LogPath != processLogRelativePath(project.Project, id) ||
				tombstone.CreatedAt.IsZero() {
				return errors.New("invalid process retention tombstone")
			}
		}
	}
	return nil
}

func validateProcessTerminalEvent(
	project monitordomain.RuntimeProjectIdentity,
	record durableProcessRecord,
) error {
	event := record.Event
	if event == nil || event.ID != processTerminalEventID(record.Process.ID) ||
		event.Kind != processTerminalEventKind || event.Project != project ||
		event.ProcessID != record.Process.ID || event.State != record.Process.State ||
		event.ExitCode != record.Process.ExitCode || event.OccurredAt.IsZero() ||
		!record.Process.State.Terminal() ||
		record.Reservation == nil ||
		!monitordomain.SameTerminalReservation(project, event.Reservation, *record.Reservation) ||
		record.Reservation.State != monitordomain.ReservationTerminalUnacknowledged {
		return errors.New("invalid process terminal event")
	}
	return nil
}

func validateRuntimeMutationSnapshot(
	snapshot runtimeMutationSnapshot,
	project monitordomain.RuntimeProjectIdentity,
) error {
	if snapshot.SchemaVersion != runtimeMutationSchemaVersion || snapshot.Project != project ||
		snapshot.Operations == nil {
		return errors.New("invalid runtime mutation journal schema")
	}
	for id, operation := range snapshot.Operations {
		if id != operation.ID {
			return errors.New("runtime mutation operation key mismatch")
		}
		if err := operation.Validate(project); err != nil {
			return err
		}
		switch operation.State {
		case "intent", "token_selected", "allocated", "authority_saved", "active", "committed",
			"release_intent", "release_marked", "authority_detached":
		default:
			return errors.New("invalid runtime mutation operation state")
		}
		if operation.AuthorityID != processAuthorityID || !safeProcessID(operation.EntityID) {
			return errors.New("invalid process runtime mutation identity")
		}
		switch operation.Kind {
		case "process_admission":
			if operation.ID != "process/"+operation.EntityID+"/admit" {
				return errors.New("invalid process admission operation id")
			}
			if operation.State != "intent" && operation.ReservationToken == "" {
				return errors.New("process admission operation lost its reservation token")
			}
		case "process_terminal_release":
			if operation.ID != "process/"+operation.EntityID+"/release" ||
				(operation.State != "release_intent" &&
					operation.State != "release_marked" &&
					operation.State != "authority_detached") {
				return errors.New("invalid process release operation")
			}
		default:
			return errors.New("invalid process runtime mutation kind")
		}
	}
	return nil
}

func processTerminalEventID(id string) string {
	return "process/" + id + "/terminal"
}

func sameDurableProcess(left, right processdomain.Process) bool {
	return reflect.DeepEqual(left, right)
}

func (runtime *processRuntimePersistence) persistenceFail(point processPersistenceFailpoint) error {
	if runtime.fail == nil {
		return nil
	}
	return runtime.fail(point)
}

func randomRuntimeID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}
