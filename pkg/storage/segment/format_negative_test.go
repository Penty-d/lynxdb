package segment

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/storage/segment/index"
)

func TestV1HeaderValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func([]byte)
		wantErr error
	}{
		{
			name: "invalid magic",
			mutate: func(data []byte) {
				copy(data[0:4], "JUNK")
			},
			wantErr: ErrInvalidMagic,
		},
		{
			name: "magic major mismatch",
			mutate: func(data []byte) {
				copy(data[0:4], "LSG2")
				binary.LittleEndian.PutUint16(data[4:6], 1)
			},
			wantErr: ErrInvalidMagic,
		},
		{
			name: "unsupported major",
			mutate: func(data []byte) {
				copy(data[0:4], "LSG9")
				binary.LittleEndian.PutUint16(data[4:6], 9)
			},
			wantErr: ErrUnsupportedMajor,
		},
		{
			name: "unsupported header revision",
			mutate: func(data []byte) {
				data[6] = 1
			},
			wantErr: ErrUnsupportedHeaderRev,
		},
		{
			name: "reserved header byte",
			mutate: func(data []byte) {
				data[7] = 0xff
			},
			wantErr: ErrCorruptSegment,
		},
		{
			name: "unknown required capability",
			mutate: func(data []byte) {
				binary.LittleEndian.PutUint64(data[8:16], 1<<63)
			},
			wantErr: ErrUnsupportedCapability,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, LSG_MIN_FILE_SIZE)
			copy(data, makeHeader(LSG_FORMAT_MAJOR_V1, 0, 0))
			tt.mutate(data)

			if err := ValidateSegmentHeader(data[:LSG_HEADER_SIZE], int64(len(data))); !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateSegmentHeader error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestV1HeaderRejectsShortFile(t *testing.T) {
	data := make([]byte, LSG_HEADER_SIZE)
	copy(data, makeHeader(LSG_FORMAT_MAJOR_V1, 0, 0))

	if err := ValidateSegmentHeader(data, int64(LSG_MIN_FILE_SIZE-1)); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("ValidateSegmentHeader error = %v, want ErrCorruptSegment", err)
	}
}

func TestV1FooterValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func([]byte)
		wantErr error
	}{
		{
			name: "bad footer magic",
			mutate: func(data []byte) {
				copy(data[0:4], "JUNK")
				rewriteFooterCRC(data)
			},
			wantErr: ErrCorruptSegment,
		},
		{
			name: "unsupported payload revision",
			mutate: func(data []byte) {
				data[4] = 1
				rewriteFooterCRC(data)
			},
			wantErr: ErrCorruptSegment,
		},
		{
			name: "nonzero reserved payload byte",
			mutate: func(data []byte) {
				data[5] = 1
				rewriteFooterCRC(data)
			},
			wantErr: ErrCorruptSegment,
		},
		{
			name: "wrong payload length",
			mutate: func(data []byte) {
				trailerStart := len(data) - LSG_FOOTER_TRAILER_SIZE
				binary.LittleEndian.PutUint32(data[trailerStart:trailerStart+4], 1)
			},
			wantErr: ErrChecksumMismatch,
		},
		{
			name: "payload tamper",
			mutate: func(data []byte) {
				data[8] ^= 1
			},
			wantErr: ErrChecksumMismatch,
		},
		{
			name: "caps summary tamper enters crc scope",
			mutate: func(data []byte) {
				trailerStart := len(data) - LSG_FOOTER_TRAILER_SIZE
				data[trailerStart+4] ^= 1
			},
			wantErr: ErrChecksumMismatch,
		},
		{
			name: "caps summary mismatch after valid crc",
			mutate: func(data []byte) {
				trailerStart := len(data) - LSG_FOOTER_TRAILER_SIZE
				data[trailerStart+4] ^= 1
				rewriteFooterCRC(data)
			},
			wantErr: ErrCorruptSegment,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := encodeFooter(&Footer{})
			tt.mutate(data)

			if _, err := DecodeFooter(data); !errors.Is(err, tt.wantErr) {
				t.Fatalf("DecodeFooter error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestV1FooterRejectsTruncatedTrailer(t *testing.T) {
	if _, err := DecodeFooter(make([]byte, LSG_FOOTER_TRAILER_SIZE-1)); !errors.Is(err, ErrCorruptSegment) {
		t.Fatalf("DecodeFooter error = %v, want ErrCorruptSegment", err)
	}
}

func TestV1PrimaryRegionHeaderValidation(t *testing.T) {
	valid := EncodePrimaryIndex(&PrimaryIndex{Interval: 1, SortFields: []string{"host"}})

	for _, tt := range []struct {
		name   string
		mutate func([]byte)
	}{
		{
			name: "magic",
			mutate: func(data []byte) {
				copy(data[0:4], "JUNK")
			},
		},
		{
			name: "revision",
			mutate: func(data []byte) {
				data[4] = 1
			},
		},
		{
			name: "reserved",
			mutate: func(data []byte) {
				data[5] = 1
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := append([]byte(nil), valid...)
			tt.mutate(data)
			if _, err := DecodePrimaryIndex(data); !errors.Is(err, ErrCorruptRegion) {
				t.Fatalf("DecodePrimaryIndex error = %v, want ErrCorruptRegion", err)
			}
		})
	}
}

func TestV1InvertedRegionHeaderValidation(t *testing.T) {
	inv := index.NewInvertedIndex()
	valid, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name   string
		mutate func([]byte)
	}{
		{
			name: "magic",
			mutate: func(data []byte) {
				copy(data[0:4], "JUNK")
			},
		},
		{
			name: "revision",
			mutate: func(data []byte) {
				data[4] = 1
			},
		},
		{
			name: "reserved",
			mutate: func(data []byte) {
				data[7] = 1
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := append([]byte(nil), valid...)
			tt.mutate(data)
			r := &Reader{
				data: data,
				footer: &Footer{
					InvertedOffset: 0,
					InvertedLength: int64(len(data)),
				},
			}
			if _, err := r.InvertedIndex(); !errors.Is(err, ErrCorruptRegion) {
				t.Fatalf("InvertedIndex error = %v, want ErrCorruptRegion", err)
			}
		})
	}
}

func TestV1BloomRegionHeaderValidation(t *testing.T) {
	valid := append(makeBloomRegionPrefix(), 0, 0)

	for _, tt := range []struct {
		name   string
		mutate func([]byte)
	}{
		{
			name: "magic",
			mutate: func(data []byte) {
				copy(data[0:4], "JUNK")
			},
		},
		{
			name: "revision",
			mutate: func(data []byte) {
				data[4] = 1
			},
		},
		{
			name: "reserved",
			mutate: func(data []byte) {
				data[7] = 1
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := append([]byte(nil), valid...)
			tt.mutate(data)
			r := &Reader{
				data: data,
				footer: &Footer{RowGroups: []RowGroupMeta{{
					PerColumnBloomOffset: 0,
					PerColumnBloomLength: int64(len(data)),
				}}},
				perColBlooms: make(map[int]map[string]*index.BloomFilter),
			}
			if _, err := r.loadPerColumnBlooms(0); !errors.Is(err, ErrCorruptRegion) {
				t.Fatalf("loadPerColumnBlooms error = %v, want ErrCorruptRegion", err)
			}
		})
	}
}

func rewriteFooterCRC(data []byte) {
	binary.LittleEndian.PutUint32(data[len(data)-4:], crc32.ChecksumIEEE(data[:len(data)-4]))
}
