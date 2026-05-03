package format

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadMarkerFileStrict(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr error
	}{
		{name: "good", content: "LSGFMT v1\n"},
		{name: "leading zero", content: "LSGFMT v01\n", wantErr: ErrCorruptMarker},
		{name: "missing newline", content: "LSGFMT v1", wantErr: ErrCorruptMarker},
		{name: "trailing whitespace", content: "LSGFMT v1 \n", wantErr: ErrCorruptMarker},
		{name: "extra line", content: "LSGFMT v1\nx\n", wantErr: ErrCorruptMarker},
		{name: "zero", content: "LSGFMT v0\n"},
		{name: "wrong magic", content: "FORMAT v1\n", wantErr: ErrCorruptMarker},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, MarkerFilename), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := ReadMarkerFile(dir)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ReadMarkerFile: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ReadMarkerFile error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestReadMarkerMismatch(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	if err := WriteMarker([]string{a}, 1); err != nil {
		t.Fatal(err)
	}
	if err := WriteMarker([]string{b}, 2); err != nil {
		t.Fatal(err)
	}

	_, err := ReadMarker([]string{b, a})
	if !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("ReadMarker error = %v, want ErrMarkerMismatch", err)
	}
	if !strings.Contains(err.Error(), "expected v1") {
		t.Fatalf("mismatch error should be deterministic against sorted first disk: %v", err)
	}
}

func TestWriteMarker(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMarker([]string{dir}, 1); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, MarkerFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "LSGFMT v1\n" {
		t.Fatalf("marker content = %q", data)
	}
	info, err := os.Stat(filepath.Join(dir, MarkerFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("marker mode = %o, want 0644", info.Mode().Perm())
	}
}
