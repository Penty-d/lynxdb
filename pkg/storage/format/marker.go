package format

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

const MarkerFilename = "FORMAT"

var (
	ErrMissingMarker  = errors.New("format marker: missing")
	ErrCorruptMarker  = errors.New("format marker: corrupt")
	ErrMarkerMismatch = errors.New("format marker: mismatch")
	ErrFutureFormat   = errors.New("format marker: future format")
	ErrAncientFormat  = errors.New("format marker: ancient format")

	markerLineRegexp = regexp.MustCompile(`^LSGFMT v([0-9]+)\n$`)
)

func ReadMarker(diskRoots []string) (uint16, error) {
	if len(diskRoots) == 0 {
		return 0, fmt.Errorf("%w: no disk roots", ErrMissingMarker)
	}
	roots := append([]string(nil), diskRoots...)
	sort.Strings(roots)

	var expected uint16
	var expectedRoot string
	for i, root := range roots {
		value, err := ReadMarkerFile(root)
		if err != nil {
			return 0, err
		}
		if i == 0 {
			expected = value
			expectedRoot = root
			continue
		}
		if value != expected {
			return 0, fmt.Errorf("%w: FORMAT marker on disk %s reports v%d; expected v%d (matching disk %s)",
				ErrMarkerMismatch, root, value, expected, expectedRoot)
		}
	}

	return expected, nil
}

func ReadMarkerFile(root string) (uint16, error) {
	path := filepath.Join(root, MarkerFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("%w: %s", ErrMissingMarker, path)
		}
		return 0, err
	}

	matches := markerLineRegexp.FindSubmatch(data)
	if matches == nil {
		return 0, fmt.Errorf("%w: FORMAT marker on disk %s does not match expected format (LSGFMT v<N>\\n)",
			ErrCorruptMarker, root)
	}

	versionText := string(matches[1])
	if len(versionText) > 1 && versionText[0] == '0' {
		return 0, fmt.Errorf("%w: FORMAT marker on disk %s does not match expected format (LSGFMT v<N>\\n)",
			ErrCorruptMarker, root)
	}
	n, err := strconv.ParseUint(versionText, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%w: FORMAT marker on disk %s does not match expected format (LSGFMT v<N>\\n)",
			ErrCorruptMarker, root)
	}
	return uint16(n), nil
}

func MarkerExists(root string) (bool, error) {
	_, err := os.Stat(filepath.Join(root, MarkerFilename))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func WriteMarker(diskRoots []string, value uint16) error {
	roots := append([]string(nil), diskRoots...)
	sort.Strings(roots)
	for _, root := range roots {
		if err := writeMarkerFile(root, value); err != nil {
			return err
		}
	}
	return nil
}

func writeMarkerFile(root string, value uint16) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	tmpPath := filepath.Join(root, MarkerFilename+".tmp")
	finalPath := filepath.Join(root, MarkerFilename)

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "LSGFMT v%d\n", value); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return err
	}

	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer func() {
		_ = dir.Close()
	}()
	return dir.Sync()
}
