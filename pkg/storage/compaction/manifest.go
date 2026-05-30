package compaction

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Manifest tracks an in-flight compaction for crash recovery.
// Written before merge starts, removed after completion.
type Manifest struct {
	FormatVersion   int       `json:"format_version"`
	ID              string    `json:"id"`
	Index           string    `json:"index"`
	Partition       string    `json:"partition,omitempty"`
	InputIDs        []string  `json:"input_ids"`
	OutputLevel     int       `json:"output_level"`
	TrivialMove     bool      `json:"trivial_move,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	OutputSegmentID string    `json:"output_segment_id,omitempty"` // set after successful completion
	CompletedAt     time.Time `json:"completed_at,omitempty"`
}

// maxHistoryEntries is the maximum number of completed manifests to retain.
const maxHistoryEntries = 1000

// ManifestStore manages compaction manifests on disk.
type ManifestStore struct {
	pendingDir    string // path to compaction/pending/ directory
	historyDir    string // path to compaction/history/ directory
	formatVersion int
	trimMu        sync.Mutex
}

// NewManifestStore creates a manifest store at the given directory.
// Creates the pending and history directories if they don't exist.
func NewManifestStore(dir string) (*ManifestStore, error) {
	return NewManifestStoreWithFormatVersion(dir, 1)
}

func NewManifestStoreWithFormatVersion(dir string, formatVersion int) (*ManifestStore, error) {
	pendingDir := filepath.Join(dir, "compaction", "pending")
	if err := os.MkdirAll(pendingDir, 0o755); err != nil {
		return nil, fmt.Errorf("compaction.NewManifestStore: create pending dir: %w", err)
	}

	historyDir := filepath.Join(dir, "compaction", "history")
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		return nil, fmt.Errorf("compaction.NewManifestStore: create history dir: %w", err)
	}

	return &ManifestStore{pendingDir: pendingDir, historyDir: historyDir, formatVersion: formatVersion}, nil
}

// Write writes a manifest for an in-flight compaction.
// Uses atomic write (tmp + rename) to prevent partial writes on crash.
func (ms *ManifestStore) Write(m *Manifest) error {
	m.FormatVersion = ms.formatVersion
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("compaction.ManifestStore.Write: marshal: %w", err)
	}

	path := filepath.Join(ms.pendingDir, m.ID+".json")
	tmpPath := path + ".tmp"

	if err := writeFileSync(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("compaction.ManifestStore.Write: write tmp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("compaction.ManifestStore.Write: rename: %w", err)
	}

	// fsync the directory so the rename survives a crash — the manifest is the
	// crash-recovery signal and must be durable once Write returns.
	if err := syncDir(ms.pendingDir); err != nil {
		return fmt.Errorf("compaction.ManifestStore.Write: sync dir: %w", err)
	}

	return nil
}

// Remove removes the manifest for a completed compaction.
func (ms *ManifestStore) Remove(id string) error {
	path := filepath.Join(ms.pendingDir, id+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("compaction.ManifestStore.Remove: %w", err)
	}

	return nil
}

// Complete moves a manifest from pending to history after successful compaction.
// The manifest should have OutputSegmentID and CompletedAt set before calling.
// Uses atomic write to history, then removes from pending.
func (ms *ManifestStore) Complete(m *Manifest) error {
	m.FormatVersion = ms.formatVersion
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("compaction.ManifestStore.Complete: marshal: %w", err)
	}

	// Atomic write to history directory. Use a unique temp file so concurrent
	// completions cannot race on a shared "<id>.json.tmp" path.
	histPath := filepath.Join(ms.historyDir, m.ID+".json")
	tmp, err := os.CreateTemp(ms.historyDir, m.ID+".json.*.tmp")
	if err != nil {
		return fmt.Errorf("compaction.ManifestStore.Complete: create history tmp: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("compaction.ManifestStore.Complete: write history: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("compaction.ManifestStore.Complete: sync history: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("compaction.ManifestStore.Complete: close history: %w", err)
	}

	if err := os.Rename(tmpPath, histPath); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("compaction.ManifestStore.Complete: rename history: %w", err)
	}
	if err := syncDir(ms.historyDir); err != nil {
		return fmt.Errorf("compaction.ManifestStore.Complete: sync history dir: %w", err)
	}

	// Remove from pending.
	_ = ms.Remove(m.ID)

	// Enforce history retention limit.
	ms.trimHistory()

	return nil
}

// LoadPending returns all pending (interrupted) compaction manifests.
// Call on startup to recover from crashes.
func (ms *ManifestStore) LoadPending() ([]*Manifest, error) {
	return ms.loadDir(ms.pendingDir)
}

// LoadHistory returns completed compaction manifests from the history directory,
// optionally filtered to entries completed after `since`. Pass zero time to load all.
func (ms *ManifestStore) LoadHistory(since time.Time) ([]*Manifest, error) {
	all, err := ms.loadDir(ms.historyDir)
	if err != nil {
		return nil, err
	}

	if since.IsZero() {
		return all, nil
	}

	var filtered []*Manifest
	for _, m := range all {
		if !m.CompletedAt.Before(since) {
			filtered = append(filtered, m)
		}
	}

	return filtered, nil
}

// loadDir reads all manifest JSON files from the given directory.
func (ms *ManifestStore) loadDir(dir string) ([]*Manifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("compaction.ManifestStore.loadDir: read dir %s: %w", dir, err)
	}

	var manifests []*Manifest

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		// Skip temp files from interrupted writes.
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue // skip unreadable files
		}

		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			continue // skip corrupt manifests
		}
		if m.FormatVersion != ms.formatVersion {
			slog.Debug("compaction.manifest: dropping manifest with mismatched format_version",
				"path", filepath.Join(dir, entry.Name()),
				"format_version", m.FormatVersion,
				"current", ms.formatVersion)
			continue
		}

		manifests = append(manifests, &m)
	}

	return manifests, nil
}

// trimHistory removes the oldest history entries if the count exceeds maxHistoryEntries.
func (ms *ManifestStore) trimHistory() {
	ms.trimMu.Lock()
	defer ms.trimMu.Unlock()

	entries, err := os.ReadDir(ms.historyDir)
	if err != nil {
		slog.Warn("manifest: failed to read history directory", "dir", ms.historyDir, "error", err)

		return
	}
	if len(entries) <= maxHistoryEntries {
		return
	}
	entries = compactManifestHistoryEntries(entries)
	if len(entries) <= maxHistoryEntries {
		return
	}

	// Sort entries by name (which includes timestamp, so oldest first).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	// Remove oldest entries beyond the retention limit.
	excess := len(entries) - maxHistoryEntries
	for i := 0; i < excess; i++ {
		if err := os.Remove(filepath.Join(ms.historyDir, entries[i].Name())); err != nil && !os.IsNotExist(err) {
			slog.Warn("manifest: failed to remove old history entry",
				"file", entries[i].Name(), "error", err)
		}
	}
}

func compactManifestHistoryEntries(entries []os.DirEntry) []os.DirEntry {
	n := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		entries[n] = entry
		n++
	}
	return entries[:n]
}

// CleanupInterrupted handles recovery for interrupted compactions. For each
// pending manifest it decides the fate of the input segments:
//
//   - If the output segment became durable (the merge wrote and registered it
//     before crashing, so outputExists reports it present), the inputs it
//     replaced are still on disk and would produce duplicate events on reload.
//     removeInputs is called to drop them.
//   - Otherwise the output never materialized — any partial output was a tmp_
//     file already removed by the filesystem scan — so the inputs are left for
//     the next compaction cycle to re-merge.
//
// The pending manifest is always removed. Returns the cleaned manifest IDs.
func (ms *ManifestStore) CleanupInterrupted(
	manifests []*Manifest,
	outputExists func(id string) bool,
	removeInputs func(m *Manifest),
) []string {
	var cleaned []string

	for _, m := range manifests {
		if m.OutputSegmentID != "" && outputExists != nil && outputExists(m.OutputSegmentID) {
			if removeInputs != nil {
				removeInputs(m)
			}
		}
		_ = ms.Remove(m.ID)
		cleaned = append(cleaned, m.ID)
	}

	return cleaned
}
