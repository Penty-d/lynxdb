package segment

import (
	"fmt"
	"sync"
)

const (
	LSG_MAGIC_V1       = "LSG1"
	LSG_FOOTER_MAGIC   = "LSGE"
	LSG_INVERTED_MAGIC = "LSIX"
	LSG_PRIMARY_MAGIC  = "LSPK"
	LSG_BLOOM_MAGIC    = "LSBL"

	LSG_HEADER_SIZE         = 24
	LSG_FOOTER_TRAILER_SIZE = 12
	LSG_MIN_FILE_SIZE       = LSG_HEADER_SIZE + LSG_FOOTER_TRAILER_SIZE + 4

	LSG_FORMAT_MAJOR_V1  uint16 = 1
	LSG_FORMAT_MAJOR_V2  uint16 = 2
	LSG_BINARY_MAX_MAJOR uint16 = 2
	LSG_BINARY_MIN_MAJOR uint16 = 1
)

var defaultFormatMajor = LSG_FORMAT_MAJOR_V2

var defaultFormatMajorMu sync.RWMutex

// ValidateFormatMajor reports whether major is supported by this binary.
func ValidateFormatMajor(major uint16) error {
	if major < LSG_BINARY_MIN_MAJOR || major > LSG_BINARY_MAX_MAJOR {
		return fmt.Errorf("%w: unsupported format major version %d (this binary supports %d..%d)",
			ErrUnsupportedMajor, major, LSG_BINARY_MIN_MAJOR, LSG_BINARY_MAX_MAJOR)
	}
	return nil
}

// SetDefaultFormatMajorForProcess overrides the process-wide fallback format
// major used by writers that do not have an explicit format major. The returned
// function restores the previous value. Prefer per-writer SetFormatMajor for
// normal production paths; this hook exists for CLI benchmarks and migration
// fixture generation.
func SetDefaultFormatMajorForProcess(major uint16) (func(), error) {
	if err := ValidateFormatMajor(major); err != nil {
		return nil, err
	}

	defaultFormatMajorMu.Lock()
	previous := defaultFormatMajor
	defaultFormatMajor = major
	defaultFormatMajorMu.Unlock()

	return func() {
		defaultFormatMajorMu.Lock()
		defaultFormatMajor = previous
		defaultFormatMajorMu.Unlock()
	}, nil
}

func currentDefaultFormatMajor() uint16 {
	defaultFormatMajorMu.RLock()
	major := defaultFormatMajor
	defaultFormatMajorMu.RUnlock()
	return major
}

func MagicForMajor(major uint16) string {
	if major > 9 {
		return ""
	}
	return string([]byte{'L', 'S', 'G', byte('0') + byte(major)})
}

func magicMajor(magic []byte) (uint16, bool) {
	if len(magic) != 4 || magic[0] != 'L' || magic[1] != 'S' || magic[2] != 'G' {
		return 0, false
	}
	if magic[3] < '0' || magic[3] > '9' {
		return 0, false
	}
	return uint16(magic[3] - '0'), true
}
