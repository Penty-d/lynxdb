package main

import (
	"errors"
	"testing"

	"github.com/lynxbase/lynxdb/internal/ui"
	storageformat "github.com/lynxbase/lynxdb/pkg/storage/format"
	"github.com/lynxbase/lynxdb/pkg/storage/segment"
)

func TestAdminFormatUpgradeNoOp(t *testing.T) {
	ui.Init(true)
	dir := t.TempDir()
	if err := storageformat.WriteMarker([]string{dir}, segment.LSG_BINARY_MAX_MAJOR); err != nil {
		t.Fatal(err)
	}

	if err := runAdminFormatUpgrade(dir, segment.LSG_BINARY_MAX_MAJOR, true); err != nil {
		t.Fatalf("runAdminFormatUpgrade: %v", err)
	}

	got, err := storageformat.ReadMarker([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if got != segment.LSG_BINARY_MAX_MAJOR {
		t.Fatalf("marker = %d, want %d", got, segment.LSG_BINARY_MAX_MAJOR)
	}
}

func TestAdminFormatUpgradeRejectsUnsupportedTarget(t *testing.T) {
	dir := t.TempDir()
	if err := storageformat.WriteMarker([]string{dir}, segment.LSG_BINARY_MAX_MAJOR); err != nil {
		t.Fatal(err)
	}

	err := runAdminFormatUpgrade(dir, segment.LSG_BINARY_MAX_MAJOR+1, true)
	if !errors.Is(err, storageformat.ErrFutureFormat) {
		t.Fatalf("error = %v, want ErrFutureFormat", err)
	}
}

func TestAdminFormatUpgradeRejectsDowngrade(t *testing.T) {
	dir := t.TempDir()
	if err := storageformat.WriteMarker([]string{dir}, segment.LSG_BINARY_MAX_MAJOR+1); err != nil {
		t.Fatal(err)
	}

	err := runAdminFormatUpgrade(dir, segment.LSG_BINARY_MAX_MAJOR, true)
	if !errors.Is(err, segment.ErrDowngradeForbidden) {
		t.Fatalf("error = %v, want ErrDowngradeForbidden", err)
	}
}
