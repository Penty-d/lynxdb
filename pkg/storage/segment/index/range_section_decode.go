package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/RoaringBitmap/roaring"
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

// RangeSectionEntryMeta is one column entry's metadata without a decoded BSI.
type RangeSectionEntryMeta struct {
	Name      string
	Layout    uint8
	BitCount  uint8
	MinValue  int64
	MaxValue  int64
	ValueKind uint8
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

// DecodeRangeSectionEntryMeta parses one named entry's metadata without
// materializing its BSI payload.
func DecodeRangeSectionEntryMeta(section []byte, columnName string) (*RangeSectionEntryMeta, error) {
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
			return &RangeSectionEntryMeta{
				Name:      header.name,
				Layout:    header.layout,
				BitCount:  header.bitCount,
				MinValue:  header.minValue,
				MaxValue:  header.maxValue,
				ValueKind: header.valueKind,
			}, nil
		}
		pos = next
	}
	if pos != len(section) {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrRangeSectionCorrupt, len(section)-pos)
	}

	return nil, nil
}

// ComparePackedRangeSectionEntry evaluates one predicate directly against a
// packed-value entry. It returns ok=false when the entry is absent or not packed.
func ComparePackedRangeSectionEntry(section []byte, columnName string, op bsi.Operation, raw int64) (*roaring.Bitmap, bool, error) {
	count, pos, err := decodeRangeSectionHeader(section)
	if err != nil {
		return nil, false, err
	}
	for i := uint16(0); i < count; i++ {
		header, next, err := decodeRangeSectionEntryHeader(section, pos, i)
		if err != nil {
			return nil, false, err
		}
		if header.name == columnName {
			if header.layout != rangeBSILayoutPackedValue {
				return nil, false, nil
			}
			mask, err := comparePackedRangePayload(header, op, raw)
			return mask, true, err
		}
		pos = next
	}
	if pos != len(section) {
		return nil, false, fmt.Errorf("%w: %d trailing bytes", ErrRangeSectionCorrupt, len(section)-pos)
	}

	return nil, false, nil
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
	switch layout {
	case rangeBSILayoutBase2, rangeBSILayoutPackedValue:
	default:
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
	switch header.layout {
	case rangeBSILayoutBase2:
		return decodeBase2RangeSectionEntryPayload(header)
	case rangeBSILayoutPackedValue:
		return decodePackedRangeSectionEntryPayload(header)
	default:
		return nil, fmt.Errorf("%w: layout %d", ErrUnsupportedRangeLayout, header.layout)
	}
}

func decodeBase2RangeSectionEntryPayload(header rangeSectionEntryHeader) (*RangeSectionEntry, error) {
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

func decodePackedRangeSectionEntryPayload(header rangeSectionEntryHeader) (*RangeSectionEntry, error) {
	idx, err := decodePackedRangePayload(header)
	if err != nil {
		return nil, err
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

func decodePackedRangePayload(header rangeSectionEntryHeader) (*bsi.BSI, error) {
	parts, err := decodePackedRangePayloadParts(header)
	if err != nil {
		return nil, err
	}
	spread := uint64(header.maxValue) - uint64(header.minValue)

	builder := NewBSIBuilder(header.valueKind, header.minValue, header.maxValue)
	valueIdx := 0
	for rowID := 0; rowID < parts.rowCount; rowID++ {
		if parts.presence[rowID/8]&(1<<uint(rowID%8)) == 0 {
			continue
		}
		if valueIdx >= parts.presentCount {
			return nil, fmt.Errorf("%w: packed presence overflows value count for %q", ErrRangeSectionCorrupt, header.name)
		}
		offset := unpackRangeValue(parts.values, uint64(valueIdx)*uint64(header.bitCount), header.bitCount)
		if offset > spread {
			return nil, fmt.Errorf("%w: packed value outside min/max for %q", ErrRangeSectionCorrupt, header.name)
		}
		builder.Set(uint32(rowID), int64(uint64(header.minValue)+offset))
		valueIdx++
	}
	if valueIdx != parts.presentCount {
		return nil, fmt.Errorf("%w: packed value count mismatch for %q", ErrRangeSectionCorrupt, header.name)
	}

	return builder.Build(), nil
}

type packedRangePayloadParts struct {
	rowCount     int
	presentCount int
	presence     []byte
	values       []byte
}

func decodePackedRangePayloadParts(header rangeSectionEntryHeader) (packedRangePayloadParts, error) {
	if len(header.payload) < 12 {
		return packedRangePayloadParts{}, fmt.Errorf("%w: packed payload header truncated for %q", ErrRangeSectionCorrupt, header.name)
	}
	rowCount := int(binary.LittleEndian.Uint32(header.payload[0:4]))
	presentCount := int(binary.LittleEndian.Uint32(header.payload[4:8]))
	presenceLen := int(binary.LittleEndian.Uint32(header.payload[8:12]))
	if presenceLen != (rowCount+7)/8 {
		return packedRangePayloadParts{}, fmt.Errorf("%w: packed presence length mismatch for %q", ErrRangeSectionCorrupt, header.name)
	}
	valueLen, err := packedValueByteLen(int(header.bitCount), presentCount)
	if err != nil {
		return packedRangePayloadParts{}, fmt.Errorf("%w: packed values too large for %q: %w", ErrRangeSectionCorrupt, header.name, err)
	}
	if 12+presenceLen+valueLen != len(header.payload) {
		return packedRangePayloadParts{}, fmt.Errorf("%w: packed payload length mismatch for %q", ErrRangeSectionCorrupt, header.name)
	}
	presence := header.payload[12 : 12+presenceLen]
	values := header.payload[12+presenceLen:]

	return packedRangePayloadParts{
		rowCount:     rowCount,
		presentCount: presentCount,
		presence:     presence,
		values:       values,
	}, nil
}

func comparePackedRangePayload(header rangeSectionEntryHeader, op bsi.Operation, raw int64) (*roaring.Bitmap, error) {
	parts, err := decodePackedRangePayloadParts(header)
	if err != nil {
		return nil, err
	}
	empty := func() *roaring.Bitmap { return roaring.New() }
	all := func() *roaring.Bitmap { return packedExistenceBitmap(parts) }

	switch op {
	case bsi.GT:
		if raw >= header.maxValue {
			return empty(), nil
		}
		if raw < header.minValue {
			return all(), nil
		}
	case bsi.GE:
		if raw > header.maxValue {
			return empty(), nil
		}
		if raw <= header.minValue {
			return all(), nil
		}
	case bsi.LT:
		if raw <= header.minValue {
			return empty(), nil
		}
		if raw > header.maxValue {
			return all(), nil
		}
	case bsi.LE:
		if raw < header.minValue {
			return empty(), nil
		}
		if raw >= header.maxValue {
			return all(), nil
		}
	default:
		return nil, fmt.Errorf("%w: unsupported packed compare op %d for %q", ErrRangeSectionCorrupt, op, header.name)
	}

	needle := uint64(raw) - uint64(header.minValue)
	spread := uint64(header.maxValue) - uint64(header.minValue)
	if needle > spread {
		return empty(), nil
	}

	mask := roaring.New()
	valueIdx := 0
	for rowID := 0; rowID < parts.rowCount; rowID++ {
		if parts.presence[rowID/8]&(1<<uint(rowID%8)) == 0 {
			continue
		}
		if valueIdx >= parts.presentCount {
			return nil, fmt.Errorf("%w: packed presence overflows value count for %q", ErrRangeSectionCorrupt, header.name)
		}
		offset := unpackRangeValue(parts.values, uint64(valueIdx)*uint64(header.bitCount), header.bitCount)
		if offset > spread {
			return nil, fmt.Errorf("%w: packed value outside min/max for %q", ErrRangeSectionCorrupt, header.name)
		}
		if packedCompareOffset(offset, op, needle) {
			mask.Add(uint32(rowID))
		}
		valueIdx++
	}
	if valueIdx != parts.presentCount {
		return nil, fmt.Errorf("%w: packed value count mismatch for %q", ErrRangeSectionCorrupt, header.name)
	}

	return mask, nil
}

func packedExistenceBitmap(parts packedRangePayloadParts) *roaring.Bitmap {
	mask := roaring.New()
	if parts.presentCount == parts.rowCount {
		mask.AddRange(0, uint64(parts.rowCount))
		return mask
	}
	for rowID := 0; rowID < parts.rowCount; rowID++ {
		if parts.presence[rowID/8]&(1<<uint(rowID%8)) != 0 {
			mask.Add(uint32(rowID))
		}
	}
	return mask
}

func packedCompareOffset(offset uint64, op bsi.Operation, needle uint64) bool {
	switch op {
	case bsi.GT:
		return offset > needle
	case bsi.GE:
		return offset >= needle
	case bsi.LT:
		return offset < needle
	case bsi.LE:
		return offset <= needle
	default:
		return false
	}
}

func unpackRangeValue(src []byte, bitOffset uint64, width uint8) uint64 {
	var value uint64
	for bit := uint8(0); bit < width; bit++ {
		pos := bitOffset + uint64(bit)
		if src[pos/8]&(1<<(pos%8)) != 0 {
			value |= uint64(1) << bit
		}
	}
	return value
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
