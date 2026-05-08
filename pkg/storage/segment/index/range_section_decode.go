package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	bsi "github.com/RoaringBitmap/roaring/BitSliceIndexing"
)

var (
	// ErrRangeSectionCorrupt reports malformed or checksum-invalid LSRB data.
	ErrRangeSectionCorrupt = errors.New("index: corrupt range BSI section")
	// ErrUnsupportedRangeLayout reports an LSRB entry layout this binary cannot decode.
	ErrUnsupportedRangeLayout = errors.New("index: unsupported range BSI layout")
)

// RangeSectionEntry is one decoded column entry from an LSRB range section.
type RangeSectionEntry struct {
	Name      string
	Layout    uint8
	BitCount  uint8
	MinValue  int64
	MaxValue  int64
	ValueKind uint8
	BSI       *bsi.BSI
}

type rangeSectionEntryHeader struct {
	name      string
	layout    uint8
	bitCount  uint8
	minValue  int64
	maxValue  int64
	valueKind uint8
	payload   []byte
}

// DecodeRangeSection parses an exact LSRB section into entries keyed by column name.
func DecodeRangeSection(section []byte) (map[string]*RangeSectionEntry, error) {
	count, pos, err := decodeRangeSectionHeader(section)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]*RangeSectionEntry, count)
	for i := uint16(0); i < count; i++ {
		header, next, err := decodeRangeSectionEntryHeader(section, pos, i)
		if err != nil {
			return nil, err
		}
		entry, err := decodeRangeSectionEntryPayload(header)
		if err != nil {
			return nil, err
		}
		entries[entry.Name] = entry
		pos = next
	}
	if pos != len(section) {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrRangeSectionCorrupt, len(section)-pos)
	}

	return entries, nil
}

// DecodeRangeSectionEntry parses one named entry from an exact LSRB section.
func DecodeRangeSectionEntry(section []byte, columnName string) (*RangeSectionEntry, error) {
	count, pos, err := decodeRangeSectionHeader(section)
	if err != nil {
		return nil, err
	}
	for i := uint16(0); i < count; i++ {
		header, next, err := decodeRangeSectionEntryHeader(section, pos, i)
		if err != nil {
			return nil, err
		}
		if header.name == columnName {
			return decodeRangeSectionEntryPayload(header)
		}
		pos = next
	}
	if pos != len(section) {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrRangeSectionCorrupt, len(section)-pos)
	}

	return nil, nil
}

func decodeRangeSectionHeader(section []byte) (uint16, int, error) {
	if len(section) < RangeSectionHeaderSize {
		return 0, 0, fmt.Errorf("%w: section header truncated", ErrRangeSectionCorrupt)
	}
	if string(section[:4]) != RangeBitmapMagic {
		return 0, 0, fmt.Errorf("%w: magic mismatch", ErrRangeSectionCorrupt)
	}
	for i := 4; i < 8; i++ {
		if section[i] != 0 {
			return 0, 0, fmt.Errorf("%w: reserved header byte set", ErrRangeSectionCorrupt)
		}
	}
	for i := 10; i < RangeSectionHeaderSize; i++ {
		if section[i] != 0 {
			return 0, 0, fmt.Errorf("%w: reserved header byte set", ErrRangeSectionCorrupt)
		}
	}

	return binary.LittleEndian.Uint16(section[8:10]), RangeSectionHeaderSize, nil
}

func decodeRangeSectionEntryHeader(section []byte, pos int, ordinal uint16) (rangeSectionEntryHeader, int, error) {
	entryStart := pos
	if pos+2 > len(section) {
		return rangeSectionEntryHeader{}, 0, fmt.Errorf("%w: entry %d name length truncated", ErrRangeSectionCorrupt, ordinal)
	}
	nameLen := int(binary.LittleEndian.Uint16(section[pos : pos+2]))
	pos += 2

	const fixedAfterName = 1 + 1 + 8 + 8 + 1 + 4
	if pos+nameLen+fixedAfterName > len(section) {
		return rangeSectionEntryHeader{}, 0, fmt.Errorf("%w: entry %d metadata truncated", ErrRangeSectionCorrupt, ordinal)
	}
	name := string(section[pos : pos+nameLen])
	pos += nameLen
	layout := section[pos]
	pos++
	bitCount := section[pos]
	pos++
	minValue := int64(binary.LittleEndian.Uint64(section[pos : pos+8]))
	pos += 8
	maxValue := int64(binary.LittleEndian.Uint64(section[pos : pos+8]))
	pos += 8
	valueKind := section[pos]
	pos++
	payloadLen := int(binary.LittleEndian.Uint32(section[pos : pos+4]))
	pos += 4
	if payloadLen < 0 || pos+payloadLen+4 > len(section) {
		return rangeSectionEntryHeader{}, 0, fmt.Errorf("%w: entry %d payload truncated", ErrRangeSectionCorrupt, ordinal)
	}
	payload := section[pos : pos+payloadLen]
	pos += payloadLen
	entryEnd := pos
	wantCRC := binary.LittleEndian.Uint32(section[pos : pos+4])
	pos += 4
	if gotCRC := crc32.ChecksumIEEE(section[entryStart:entryEnd]); gotCRC != wantCRC {
		return rangeSectionEntryHeader{}, 0, fmt.Errorf("%w: entry %q crc mismatch", ErrRangeSectionCorrupt, name)
	}
	if layout != rangeBSILayoutBase2 {
		return rangeSectionEntryHeader{}, 0, fmt.Errorf("%w: layout %d", ErrUnsupportedRangeLayout, layout)
	}

	return rangeSectionEntryHeader{
		name:      name,
		layout:    layout,
		bitCount:  bitCount,
		minValue:  minValue,
		maxValue:  maxValue,
		valueKind: valueKind,
		payload:   payload,
	}, pos, nil
}

func decodeRangeSectionEntryPayload(header rangeSectionEntryHeader) (*RangeSectionEntry, error) {
	frames, err := decodeRangeSectionFrames(header.name, header.payload)
	if err != nil {
		return nil, err
	}
	idx := bsi.NewDefaultBSI()
	if err := idx.UnmarshalBinary(frames); err != nil {
		return nil, fmt.Errorf("%w: unmarshal BSI %q: %w", ErrRangeSectionCorrupt, header.name, err)
	}

	return &RangeSectionEntry{
		Name:      header.name,
		Layout:    header.layout,
		BitCount:  header.bitCount,
		MinValue:  header.minValue,
		MaxValue:  header.maxValue,
		ValueKind: header.valueKind,
		BSI:       idx,
	}, nil
}

func decodeRangeSectionFrames(name string, payload []byte) ([][]byte, error) {
	frames := make([][]byte, 0)
	pos := 0
	for pos < len(payload) {
		if pos+4 > len(payload) {
			return nil, fmt.Errorf("%w: frame length truncated for %q", ErrRangeSectionCorrupt, name)
		}
		frameLen := int(binary.LittleEndian.Uint32(payload[pos : pos+4]))
		pos += 4
		if frameLen < 0 || pos+frameLen > len(payload) {
			return nil, fmt.Errorf("%w: frame data truncated for %q", ErrRangeSectionCorrupt, name)
		}
		frames = append(frames, payload[pos:pos+frameLen])
		pos += frameLen
	}

	return frames, nil
}
