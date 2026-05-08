package index

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"

	bsi "github.com/RoaringBitmap/roaring/BitSliceIndexing"
)

type parsedRangeSectionForTest struct {
	Count   uint16
	Entries []parsedRangeEntryForTest
}

type parsedRangeEntryForTest struct {
	Name       string
	Layout     uint8
	BitCount   uint8
	MinValue   int64
	MaxValue   int64
	ValueKind  uint8
	Payload    []byte
	CRC        uint32
	crcPayload []byte
}

func TestUnit_RangeSectionEncoder_TwoColumns_EncodesDocumentedLayout(t *testing.T) {
	var buf bytes.Buffer
	enc := NewRangeSectionEncoder(&buf, 123)

	intBuilder := NewBSIBuilder(RangeBSIValueInt, 10, 20)
	intBuilder.Set(0, 10)
	intBuilder.Set(3, 19)
	if err := enc.AddColumn("duration_ms", RangeBSIValueInt, 10, 20, intBuilder.Build()); err != nil {
		t.Fatalf("AddColumn(int): %v", err)
	}

	minFloat := FloatToOrderedInt64(-1.5)
	maxFloat := FloatToOrderedInt64(2.25)
	floatBuilder := NewBSIBuilder(RangeBSIValueFloat64Bits, minFloat, maxFloat)
	floatBuilder.Set(1, FloatToOrderedInt64(-1.5))
	floatBuilder.Set(2, FloatToOrderedInt64(2.25))
	if err := enc.AddColumn("latency", RangeBSIValueFloat64Bits, minFloat, maxFloat, floatBuilder.Build()); err != nil {
		t.Fatalf("AddColumn(float): %v", err)
	}

	off, length, err := enc.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if off != 123 {
		t.Fatalf("offset = %d, want 123", off)
	}
	if length != int64(buf.Len()) {
		t.Fatalf("length = %d, want %d", length, buf.Len())
	}

	section := parseRangeSectionForTest(t, buf.Bytes())
	if section.Count != 2 {
		t.Fatalf("bsiCount = %d, want 2", section.Count)
	}

	first := section.Entries[0]
	if first.Name != "duration_ms" || first.Layout != 0 || first.ValueKind != RangeBSIValueInt ||
		first.MinValue != 10 || first.MaxValue != 20 {
		t.Fatalf("first entry = %+v", first)
	}
	if first.BitCount != uint8(intBuilder.BitCount()) {
		t.Fatalf("first bitCount = %d, want %d", first.BitCount, intBuilder.BitCount())
	}
	assertRangeEntryCRCValid(t, first)

	second := section.Entries[1]
	if second.Name != "latency" || second.Layout != 0 || second.ValueKind != RangeBSIValueFloat64Bits ||
		second.MinValue != minFloat || second.MaxValue != maxFloat {
		t.Fatalf("second entry = %+v", second)
	}
	if second.BitCount != uint8(floatBuilder.BitCount()) {
		t.Fatalf("second bitCount = %d, want %d", second.BitCount, floatBuilder.BitCount())
	}
	assertRangeEntryCRCValid(t, second)
}

func TestUnit_RangeSectionEncoder_CRCDetectsPayloadMutation(t *testing.T) {
	var buf bytes.Buffer
	enc := NewRangeSectionEncoder(&buf, 0)
	builder := NewBSIBuilder(RangeBSIValueInt, 1, 9)
	builder.Set(0, 1)
	builder.Set(1, 9)
	if err := enc.AddColumn("status", RangeBSIValueInt, 1, 9, builder.Build()); err != nil {
		t.Fatalf("AddColumn: %v", err)
	}
	if _, _, err := enc.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	section := parseRangeSectionForTest(t, buf.Bytes())
	entry := section.Entries[0]
	assertRangeEntryCRCValid(t, entry)

	mutated := append([]byte(nil), entry.crcPayload...)
	mutated[len(mutated)-1] ^= 0xff
	if crc32.ChecksumIEEE(mutated) == entry.CRC {
		t.Fatal("CRC did not detect payload mutation")
	}
}

func TestUnit_RangeSectionEncoder_EmptySection_EncodesFixedHeader(t *testing.T) {
	var buf bytes.Buffer
	enc := NewRangeSectionEncoder(&buf, 99)
	off, length, err := enc.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if off != 99 {
		t.Fatalf("offset = %d, want 99", off)
	}
	if length != RangeSectionHeaderSize {
		t.Fatalf("length = %d, want %d", length, RangeSectionHeaderSize)
	}
	want := []byte{
		'L', 'S', 'R', 'B',
		0, 0, 0, 0,
		0, 0,
		0, 0, 0, 0, 0, 0,
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("empty section bytes = %x, want %x", buf.Bytes(), want)
	}
}

func TestUnit_RangeSectionEncoder_BSIFrames_RoundTripThroughUnmarshalBinary(t *testing.T) {
	const rows = 8192
	builder := NewBSIBuilder(RangeBSIValueInt, 0, 1<<40)
	want := make(map[uint64]int64, rows)
	for row := 0; row < rows; row++ {
		raw := int64((uint64(row)*7919 + 17) & ((1 << 40) - 1))
		builder.Set(uint32(row), raw)
		want[uint64(row)] = raw
	}

	var buf bytes.Buffer
	enc := NewRangeSectionEncoder(&buf, 0)
	if err := enc.AddColumn("bytes", RangeBSIValueInt, 0, 1<<40, builder.Build()); err != nil {
		t.Fatalf("AddColumn: %v", err)
	}
	if _, _, err := enc.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	section := parseRangeSectionForTest(t, buf.Bytes())
	entry := section.Entries[0]
	decoded := decodeBSIFramesForTest(t, entry.Payload)
	if got, wantCard := decoded.GetExistenceBitmap().GetCardinality(), uint64(rows); got != wantCard {
		t.Fatalf("decoded existence cardinality = %d, want %d", got, wantCard)
	}

	for row := uint64(0); row < rows; row++ {
		got, ok := decoded.GetValue(row)
		if !ok {
			t.Fatalf("decoded GetValue(%d) missing", row)
		}
		if got != want[row] {
			t.Fatalf("decoded GetValue(%d) = %d, want %d", row, got, want[row])
		}
	}
}

func parseRangeSectionForTest(t *testing.T, data []byte) parsedRangeSectionForTest {
	t.Helper()
	if len(data) < RangeSectionHeaderSize {
		t.Fatalf("section length = %d, want at least %d", len(data), RangeSectionHeaderSize)
	}
	if string(data[:4]) != RangeBitmapMagic {
		t.Fatalf("magic = %q, want %q", data[:4], RangeBitmapMagic)
	}
	if !bytes.Equal(data[4:8], []byte{0, 0, 0, 0}) {
		t.Fatalf("reserved bytes = %x, want zeroes", data[4:8])
	}
	count := binary.LittleEndian.Uint16(data[8:10])
	if !bytes.Equal(data[10:RangeSectionHeaderSize], []byte{0, 0, 0, 0, 0, 0}) {
		t.Fatalf("padding bytes = %x, want zeroes", data[10:RangeSectionHeaderSize])
	}

	pos := RangeSectionHeaderSize
	entries := make([]parsedRangeEntryForTest, 0, count)
	for i := uint16(0); i < count; i++ {
		entryStart := pos
		if pos+2 > len(data) {
			t.Fatalf("entry %d truncated before name length", i)
		}
		nameLen := int(binary.LittleEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if pos+nameLen+1+1+8+8+1+4 > len(data) {
			t.Fatalf("entry %d truncated before payload", i)
		}
		name := string(data[pos : pos+nameLen])
		pos += nameLen
		layout := data[pos]
		pos++
		bitCount := data[pos]
		pos++
		minValue := int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
		pos += 8
		maxValue := int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
		pos += 8
		valueKind := data[pos]
		pos++
		payloadLen := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
		pos += 4
		if pos+payloadLen+4 > len(data) {
			t.Fatalf("entry %d truncated in payload: need %d bytes at %d of %d", i, payloadLen, pos, len(data))
		}
		payload := append([]byte(nil), data[pos:pos+payloadLen]...)
		pos += payloadLen
		crcPayload := append([]byte(nil), data[entryStart:pos]...)
		crc := binary.LittleEndian.Uint32(data[pos : pos+4])
		pos += 4

		entries = append(entries, parsedRangeEntryForTest{
			Name:       name,
			Layout:     layout,
			BitCount:   bitCount,
			MinValue:   minValue,
			MaxValue:   maxValue,
			ValueKind:  valueKind,
			Payload:    payload,
			CRC:        crc,
			crcPayload: crcPayload,
		})
	}
	if pos != len(data) {
		t.Fatalf("section has %d trailing bytes", len(data)-pos)
	}
	return parsedRangeSectionForTest{Count: count, Entries: entries}
}

func assertRangeEntryCRCValid(t *testing.T, entry parsedRangeEntryForTest) {
	t.Helper()
	if got := crc32.ChecksumIEEE(entry.crcPayload); got != entry.CRC {
		t.Fatalf("entry %q CRC = %#x, want %#x", entry.Name, got, entry.CRC)
	}
}

func decodeBSIFramesForTest(t *testing.T, payload []byte) *bsi.BSI {
	t.Helper()
	var frames [][]byte
	pos := 0
	for pos < len(payload) {
		if pos+4 > len(payload) {
			t.Fatalf("payload truncated before frame length at %d", pos)
		}
		frameLen := int(binary.LittleEndian.Uint32(payload[pos : pos+4]))
		pos += 4
		if pos+frameLen > len(payload) {
			t.Fatalf("payload truncated in frame: need %d bytes at %d of %d", frameLen, pos, len(payload))
		}
		frames = append(frames, append([]byte(nil), payload[pos:pos+frameLen]...))
		pos += frameLen
	}
	decoded := bsi.NewDefaultBSI()
	if err := decoded.UnmarshalBinary(frames); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	return decoded
}
