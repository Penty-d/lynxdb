package index

import bsi "github.com/RoaringBitmap/roaring/BitSliceIndexing"

// BSIBuilder builds one row group's bit-sliced range index for one column.
type BSIBuilder struct {
	valueKind uint8
	minValue  int64
	bsi       *bsi.BSI
}

// NewBSIBuilder allocates a builder against a fixed raw [minValue, maxValue]
// range. Row IDs passed to Set are row-group local, starting at zero; segment
// global row translation is handled by the scanner when BSI is consulted.
func NewBSIBuilder(valueKind uint8, minValue, maxValue int64) *BSIBuilder {
	maxOffset := int64(uint64(maxValue) - uint64(minValue))
	return &BSIBuilder{
		valueKind: valueKind,
		minValue:  minValue,
		bsi:       bsi.NewBSI(maxOffset, 0),
	}
}

// Set records a raw column value for a row-group-local row ID.
func (b *BSIBuilder) Set(rowID uint32, raw int64) {
	b.bsi.SetValue(uint64(rowID), int64(uint64(raw)-uint64(b.minValue)))
}

// BitCount returns the number of BSI slices.
func (b *BSIBuilder) BitCount() int {
	return b.bsi.BitCount()
}

func (b *BSIBuilder) Build() *bsi.BSI {
	return b.bsi
}

// ValueKind returns the encoded value kind for this BSI.
func (b *BSIBuilder) ValueKind() uint8 {
	return b.valueKind
}
