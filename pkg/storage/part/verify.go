package part

import (
	"fmt"

	"github.com/lynxbase/lynxdb/pkg/storage/segment"
)

func verifySegmentBeforePromote(path string) (formatMajor uint16, bsiColumns int, bsiSectionBytes int64, err error) {
	ms, err := segment.OpenSegmentFile(path)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("verify open segment: %w", err)
	}
	defer func() {
		if closeErr := ms.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("verify close segment: %w", closeErr)
		}
	}()

	reader := ms.Reader()
	if err := reader.VerifyAllRangeBSIs(); err != nil {
		return 0, 0, 0, fmt.Errorf("verify range BSI: %w", err)
	}

	formatMajor = reader.FormatMajor()
	bsiColumns, bsiSectionBytes = reader.RangeBSIStats()
	return formatMajor, bsiColumns, bsiSectionBytes, nil
}
