package segment

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCrossVersionReadFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "segments", "v*.lsg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no segment fixtures found")
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			r, err := OpenSegment(data)
			if err != nil {
				t.Fatalf("OpenSegment: %v", err)
			}
			if r.EventCount() == 0 {
				t.Fatal("fixture has no events")
			}
			if string(data[:4]) != LSG_MAGIC_V1 {
				t.Fatalf("magic = %q, want %q", data[:4], LSG_MAGIC_V1)
			}
		})
	}
}

func TestFixtureHeaderCapsMatchFooter(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "segments", "v*.lsg"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			footer, err := DecodeFooter(data)
			if err != nil {
				t.Fatal(err)
			}
			req, opt := aggregateCapabilities(footer.RowGroups)
			if req != footer.RequiredCaps || opt != footer.OptionalCaps {
				t.Fatalf("footer caps = (%#x,%#x), row groups = (%#x,%#x)", footer.RequiredCaps, footer.OptionalCaps, req, opt)
			}
		})
	}
}
