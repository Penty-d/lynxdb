package compaction

import (
	"fmt"
	"os"
)

// writeFileSync writes data to path and fsyncs the file so its contents are
// durable on disk before the caller renames it into place.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()

		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()

		return err
	}

	return f.Close()
}

// syncDir fsyncs a directory so that metadata updates such as rename are durable
// on filesystems that require the directory entry itself to be synced.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir: %w", err)
	}
	defer f.Close()

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync dir: %w", err)
	}

	return nil
}
