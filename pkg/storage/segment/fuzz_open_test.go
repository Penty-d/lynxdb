package segment

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func FuzzOpenSegment(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "segments", "v*.lsg"))
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err == nil {
			f.Add(data)
		}
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		_, err := OpenSegment(data)
		if err != nil && !isKnownOpenError(err) {
			t.Fatalf("unexpected error type: %v", err)
		}
	})
}

func isKnownOpenError(err error) bool {
	return errors.Is(err, ErrInvalidMagic) ||
		errors.Is(err, ErrUnsupportedMajor) ||
		errors.Is(err, ErrUnsupportedCapability) ||
		errors.Is(err, ErrUnsupportedHeaderRev) ||
		errors.Is(err, ErrChecksumMismatch) ||
		errors.Is(err, ErrCorruptSegment) ||
		errors.Is(err, ErrCorruptRegion)
}
