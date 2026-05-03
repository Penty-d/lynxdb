package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	storageformat "github.com/lynxbase/lynxdb/pkg/storage/format"
	"github.com/lynxbase/lynxdb/pkg/storage/part"
	"github.com/lynxbase/lynxdb/pkg/storage/segment"
)

func newAdminFormatUpgradeCmd() *cobra.Command {
	var dataDir string
	var target uint16
	var confirm bool

	cmd := &cobra.Command{
		Use:   "format-upgrade --data-dir DIR --to N --confirm",
		Short: "Ratchet the local data directory format marker",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runAdminFormatUpgrade(dataDir, target, confirm)
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "Root data directory")
	cmd.Flags().Uint16Var(&target, "to", 0, "Target format major version")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Confirm the format marker change")
	_ = cmd.MarkFlagRequired("data-dir")
	_ = cmd.MarkFlagRequired("to")

	return cmd
}

func runAdminFormatUpgrade(dataDir string, target uint16, confirm bool) error {
	if target < segment.LSG_BINARY_MIN_MAJOR || target > segment.LSG_BINARY_MAX_MAJOR {
		return fmt.Errorf("%w: target version %d is outside binary's supported range [%d, %d]",
			storageformat.ErrFutureFormat, target, segment.LSG_BINARY_MIN_MAJOR, segment.LSG_BINARY_MAX_MAJOR)
	}

	current, err := storageformat.ReadMarker([]string{dataDir})
	if err != nil {
		return err
	}
	if target < current {
		return fmt.Errorf("%w: cannot write segment at major %d; data dir is ratcheted to major %d",
			segment.ErrDowngradeForbidden, target, current)
	}
	if target == current {
		printSuccess("FORMAT already at v%d; no change", current)
		return nil
	}
	if !confirm {
		return fmt.Errorf("format-upgrade requires --confirm")
	}

	segments, err := listAdminSegmentFiles(dataDir)
	if err != nil {
		return err
	}
	for _, path := range segments {
		major, err := readAdminSegmentMajor(path)
		if err != nil {
			return fmt.Errorf("scan segment %s: %w", path, err)
		}
		if major != current && major > target {
			return fmt.Errorf("%w: segment %s is at major %d, current marker is %d, target is %d",
				segment.ErrUnsupportedMajor, path, major, current, target)
		}
	}

	if err := storageformat.WriteMarker([]string{dataDir}, target); err != nil {
		return err
	}
	printSuccess("FORMAT upgraded from v%d to v%d", current, target)
	return nil
}

func listAdminSegmentFiles(dataDir string) ([]string, error) {
	root := filepath.Join(dataDir, "segments")
	var paths []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".lsg" {
			return nil
		}
		name := filepath.Base(path)
		if part.IsTempFile(name) || part.IsDeletedFile(name) {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return paths, nil
}

func readAdminSegmentMajor(path string) (uint16, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	header := make([]byte, segment.LSG_HEADER_SIZE)
	n, err := io.ReadFull(f, header)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return segment.SegmentHeaderMajor(header[:n], info.Size())
		}
		return 0, err
	}
	return segment.SegmentHeaderMajor(header, info.Size())
}
