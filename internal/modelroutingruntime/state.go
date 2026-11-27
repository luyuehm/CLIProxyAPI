package modelroutingruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelrouting"
)

// stateFileVersion is the persisted document version. A document with a
// different version is refused rather than guessed at, so a future format
// change cannot be silently misread as this one.
const stateFileVersion = 1

// stateFile is the durable form of the strategy version history. It preserves
// every record verbatim (version number, timestamp, note and full strategy
// snapshot), so a restart restores the audit trail rather than re-deriving it.
type stateFile struct {
	Version int          `json:"version"`
	Scopes  []stateScope `json:"scopes"`
}

type stateScope struct {
	Kind     string                 `json:"kind"`
	ID       string                 `json:"id"`
	Versions []modelrouting.Version `json:"versions"`
}

// loadState reads the persisted history into the runtime's version table and
// scope index. A missing file is the normal first-boot case (no error). A
// corrupt or unsupported file is reported so the caller can log it loudly: the
// channel keeps running with an empty history rather than refusing to boot,
// because a bad management-state file must not take the data plane down.
func (r *Runtime) loadState(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var doc stateFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Version != stateFileVersion {
		return fmt.Errorf("parse %s: unsupported state version %d (want %d)",
			path, doc.Version, stateFileVersion)
	}
	// Record how many scopes the file holds so a mid-list failure can report
	// "restored N of M" rather than implying everything loaded.
	r.totalScopes = len(doc.Scopes)
	for _, scope := range doc.Scopes {
		key := modelrouting.Scope{Kind: scope.Kind, ID: scope.ID}
		if err := r.core.Versions.Restore(key, scope.Versions); err != nil {
			return fmt.Errorf("restore %s: %w", path, err)
		}
		r.trackScope(key)
		r.restoredScopes++
	}
	return nil
}

// persist writes the whole history atomically: a temp file in the same
// directory plus a rename, so a crash mid-write can never leave a truncated
// document that a restart would then read as "no policies".
//
// A file that failed to load is never overwritten. If it did, the first policy
// edit after a damaged read would erase every other scope's history — the
// operator would lose the very data the error message told them to reconcile.
// Instead the new document goes to a sibling "<path>.recovered-<unix>" file and
// the caller reports where it went.
//
// It returns the write error unchanged; the caller decides what to tell the
// operator. An empty path means the in-memory-only deployment, which has
// nothing to write.
func (r *Runtime) persist() (string, error) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	path := r.statePath
	if path == "" {
		return "", nil
	}
	if r.loadFailed {
		path = path + ".recovered-" + strconv.FormatInt(time.Now().Unix(), 10)
	}

	doc := stateFile{Version: stateFileVersion}
	for scope := range r.scopes {
		doc.Scopes = append(doc.Scopes, stateScope{
			Kind:     scope.Kind,
			ID:       scope.ID,
			Versions: r.core.Versions.List(scope),
		})
	}
	// Stable ordering keeps the file diffable and its content deterministic.
	sortScopes(doc.Scopes)

	payload, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode state: %w", err)
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", fmt.Errorf("create state dir: %w", err)
		}
	}
	temp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp state: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		// Best-effort cleanup; the rename below removes the normal path.
		_ = os.Remove(tempName)
	}()
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("write temp state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("sync temp state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Chmod(tempName, 0o600); err != nil {
		return "", fmt.Errorf("chmod temp state: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return "", fmt.Errorf("rename state: %w", err)
	}
	return path, nil
}

// trackScope records that a scope has a history worth persisting.
func (r *Runtime) trackScope(scope modelrouting.Scope) {
	if scope.Kind == "" || scope.ID == "" {
		return
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.scopes == nil {
		r.scopes = make(map[modelrouting.Scope]struct{})
	}
	r.scopes[scope] = struct{}{}
}

// sortScopes orders persisted scopes by kind then id.
func sortScopes(scopes []stateScope) {
	less := func(i, j int) bool {
		if scopes[i].Kind != scopes[j].Kind {
			return scopes[i].Kind < scopes[j].Kind
		}
		return scopes[i].ID < scopes[j].ID
	}
	// Insertion sort: the slice is tiny (one entry per configured scope) and
	// this keeps the package free of an extra import for a 3-line need.
	for i := 1; i < len(scopes); i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			scopes[j], scopes[j-1] = scopes[j-1], scopes[j]
		}
	}
}
