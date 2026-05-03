package compaction

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("compaction.ManifestStore.Write: write tmp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("compaction.ManifestStore.Write: rename: %w", err)
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

	// Atomic write to history directory.
	histPath := filepath.Join(ms.historyDir, m.ID+".json")
	tmpPath := histPath + ".tmp"

	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return fmt.Errorf("compaction.ManifestStore.Complete: write history: %w", err)
	}

	if err := os.Rename(tmpPath, histPath); err != nil {
		os.Remove(tmpPath)

		return fmt.Errorf("compaction.ManifestStore.Complete: rename history: %w", err)
	}

	// Remove from pending.
	ms.Remove(m.ID)

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
	entries, err := os.ReadDir(ms.historyDir)
	if err != nil {
		slog.Warn("manifest: failed to read history directory", "dir", ms.historyDir, "error", err)

		return
	}
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
		if err := os.Remove(filepath.Join(ms.historyDir, entries[i].Name())); err != nil {
			slog.Warn("manifest: failed to remove old history entry",
				"file", entries[i].Name(), "error", err)
		}
	}
}

// CleanupInterrupted handles recovery for interrupted compactions.
// For each pending manifest, it removes the manifest file. The actual segment
// cleanup is handled by the filesystem scan (which ignores tmp_ files) and the
// next compaction cycle (which will re-plan if needed). This is the safe,
// conservative recovery path.
func (ms *ManifestStore) CleanupInterrupted(manifests []*Manifest, existsFn func(id string) bool) []string {
	var cleaned []string

	for _, m := range manifests {
		ms.Remove(m.ID)
		cleaned = append(cleaned, m.ID)
	}

	return cleaned
}
