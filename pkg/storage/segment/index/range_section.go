package index

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"

	bsi "github.com/RoaringBitmap/roaring/BitSliceIndexing"
)

const (
	// RangeBitmapMagic identifies a per-row-group range bitmap section.
	RangeBitmapMagic = "LSRB"
	// RangeSectionHeaderSize is the fixed LSRB section header size.
	RangeSectionHeaderSize    = 16
	rangeBSILayoutBase2       = 0
	rangeBSILayoutPackedValue = 2
)

// RangeSectionValue is one row-group-local value encoded in a compact LSRB
// payload. Offset is the unsigned raw value offset from the entry min value.
type RangeSectionValue struct {
	RowID  uint32
	Offset uint64
}

// RangeSectionEncoder serializes a row group's range BSI columns.
type RangeSectionEncoder struct {
	w     io.Writer
	start int64
	n     uint16
	buf   bytes.Buffer
}

// NewRangeSectionEncoder creates an encoder that writes to w at startOffset.
func NewRangeSectionEncoder(w io.Writer, startOffset int64) *RangeSectionEncoder {
	return &RangeSectionEncoder{w: w, start: startOffset}
}

// AddColumn appends one column BSI to the section.
func (e *RangeSectionEncoder) AddColumn(name string, kind uint8, minValue, maxValue int64, idx *bsi.BSI) error {
	if idx == nil {
		return fmt.Errorf("index: encode range BSI column %q: nil BSI", name)
	}
	bitCount := idx.BitCount()
	if bitCount > math.MaxUint8 {
		return fmt.Errorf("index: encode range BSI column %q: bit count %d exceeds uint8", name, bitCount)
	}

	frames, err := idx.MarshalBinary()
	if err != nil {
		return fmt.Errorf("index: marshal range BSI column %q: %w", name, err)
	}
	var payload bytes.Buffer
	for _, frame := range frames {
		if len(frame) > math.MaxUint32 {
			return fmt.Errorf("index: encode range BSI column %q: frame too large", name)
		}
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(frame)))
		payload.Write(lenBuf[:])
		payload.Write(frame)
	}
	if payload.Len() > math.MaxUint32 {
		return fmt.Errorf("index: encode range BSI column %q: payload too large", name)
	}

	return e.addEntry(name, rangeBSILayoutBase2, uint8(bitCount), kind, minValue, maxValue, payload.Bytes())
}

// AddColumnPacked appends one column as row-local bit-packed value offsets.
// This layout is more compact than serialized BSI frames for dense, high-spread
// numeric row groups; readers reconstruct the in-memory BSI on demand.
func (e *RangeSectionEncoder) AddColumnPacked(name string, kind uint8, minValue, maxValue int64, bitCount, rowCount int, values []RangeSectionValue) error {
	if bitCount < 0 || bitCount > math.MaxUint8 {
		return fmt.Errorf("index: encode packed range BSI column %q: bit count %d exceeds uint8", name, bitCount)
	}
	if rowCount < 0 || rowCount > math.MaxUint32 {
		return fmt.Errorf("index: encode packed range BSI column %q: row count out of range", name)
	}
	if len(values) > math.MaxUint32 {
		return fmt.Errorf("index: encode packed range BSI column %q: too many values", name)
	}

	payload, err := encodePackedRangePayload(bitCount, rowCount, values)
	if err != nil {
		return fmt.Errorf("index: encode packed range BSI column %q: %w", name, err)
	}
	if len(payload) > math.MaxUint32 {
		return fmt.Errorf("index: encode packed range BSI column %q: payload too large", name)
	}

	return e.addEntry(name, rangeBSILayoutPackedValue, uint8(bitCount), kind, minValue, maxValue, payload)
}

func (e *RangeSectionEncoder) addEntry(name string, layout, bitCount, kind uint8, minValue, maxValue int64, payload []byte) error {
	if e.n == math.MaxUint16 {
		return fmt.Errorf("index: encode range BSI column %q: too many columns", name)
	}
	nameBytes := []byte(name)
	if len(nameBytes) > math.MaxUint16 {
		return fmt.Errorf("index: encode range BSI column %q: name too long", name)
	}

	var entry bytes.Buffer
	var lenBuf [2]byte
	binary.LittleEndian.PutUint16(lenBuf[:], uint16(len(nameBytes)))
	entry.Write(lenBuf[:])
	entry.Write(nameBytes)
	entry.WriteByte(layout)
	entry.WriteByte(bitCount)
	var numBuf [8]byte
	binary.LittleEndian.PutUint64(numBuf[:], uint64(minValue))
	entry.Write(numBuf[:])
	binary.LittleEndian.PutUint64(numBuf[:], uint64(maxValue))
	entry.Write(numBuf[:])
	entry.WriteByte(kind)
	var payloadLen [4]byte
	binary.LittleEndian.PutUint32(payloadLen[:], uint32(len(payload)))
	entry.Write(payloadLen[:])
	entry.Write(payload)

	crc := crc32.ChecksumIEEE(entry.Bytes())
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc)
	entry.Write(crcBuf[:])

	e.buf.Write(entry.Bytes())
	e.n++
	return nil
}

func encodePackedRangePayload(bitCount, rowCount int, values []RangeSectionValue) ([]byte, error) {
	presenceBytes := (rowCount + 7) / 8
	valueBytes, err := packedValueByteLen(bitCount, len(values))
	if err != nil {
		return nil, err
	}

	payload := make([]byte, 0, 12+presenceBytes+valueBytes)
	payload = binary.LittleEndian.AppendUint32(payload, uint32(rowCount))
	payload = binary.LittleEndian.AppendUint32(payload, uint32(len(values)))
	payload = binary.LittleEndian.AppendUint32(payload, uint32(presenceBytes))
	presenceStart := len(payload)
	payload = append(payload, make([]byte, presenceBytes+valueBytes)...)
	valuesStart := presenceStart + presenceBytes

	var lastRow uint32
	for i, value := range values {
		if int(value.RowID) >= rowCount {
			return nil, fmt.Errorf("row id %d outside row count %d", value.RowID, rowCount)
		}
		if i > 0 && value.RowID <= lastRow {
			return nil, fmt.Errorf("row ids must be strictly increasing")
		}
		lastRow = value.RowID
		payload[presenceStart+int(value.RowID/8)] |= 1 << (value.RowID % 8)
		packRangeValue(payload[valuesStart:], uint64(i*bitCount), uint8(bitCount), value.Offset)
	}

	return payload, nil
}

func packedValueByteLen(bitCount, valueCount int) (int, error) {
	if bitCount < 0 || valueCount < 0 {
		return 0, fmt.Errorf("negative packed value dimensions")
	}
	if bitCount == 0 || valueCount == 0 {
		return 0, nil
	}
	bitsTotal := uint64(bitCount) * uint64(valueCount)
	if bitsTotal > uint64(math.MaxInt) {
		return 0, fmt.Errorf("packed values too large")
	}
	return int((bitsTotal + 7) / 8), nil
}

func packRangeValue(dst []byte, bitOffset uint64, width uint8, value uint64) {
	for bit := uint8(0); bit < width; bit++ {
		if (value>>bit)&1 == 0 {
			continue
		}
		pos := bitOffset + uint64(bit)
		dst[pos/8] |= 1 << (pos % 8)
	}
}

// Finalize writes the section and returns its file offset and byte length.
func (e *RangeSectionEncoder) Finalize() (offset, length int64, err error) {
	section := make([]byte, 0, RangeSectionHeaderSize+e.buf.Len())
	section = append(section, RangeBitmapMagic...)
	section = append(section, 0, 0, 0, 0)
	section = binary.LittleEndian.AppendUint16(section, e.n)
	section = append(section, 0, 0, 0, 0, 0, 0)
	section = append(section, e.buf.Bytes()...)

	if _, err := e.w.Write(section); err != nil {
		return e.start, int64(len(section)), fmt.Errorf("index: write range BSI section: %w", err)
	}
	return e.start, int64(len(section)), nil
}
