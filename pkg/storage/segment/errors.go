package segment

import "errors"

var (
	ErrCorruptSegment        = errors.New("segment: corrupt data")
	ErrChecksumMismatch      = errors.New("segment: checksum mismatch")
	ErrNoEvents              = errors.New("segment: no events to write")
	ErrColumnNotFound        = errors.New("segment: column not found")
	ErrInvalidMagic          = errors.New("segment: invalid magic bytes")
	ErrUnsupportedMajor      = errors.New("segment: unsupported format major version")
	ErrUnsupportedCapability = errors.New("segment: unsupported required or optional capability")
	ErrUnsupportedHeaderRev  = errors.New("segment: unsupported header revision")
	ErrCorruptRegion         = errors.New("segment: corrupt region")
	ErrDowngradeForbidden    = errors.New("segment: downgrade forbidden")
	ErrInvalidRGIndex        = errors.New("segment: invalid row group index")
)
