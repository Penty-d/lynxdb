package pipeline

import (
	"container/heap"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/vm"
)

// SpillWriter writes sorted partial results to a temporary file.
type SpillWriter struct {
	file *os.File
	enc  *msgpack.Encoder
	rows int
	mgr  *SpillManager // nil = unmanaged (legacy path)
}

// NewSpillWriter creates a new spill writer with a temporary file.
// Legacy constructor without SpillManager — prefer NewManagedSpillWriter.
func NewSpillWriter() (*SpillWriter, error) {
	f, err := os.CreateTemp("", "lynxdb-spill-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("spill: create temp: %w", err)
	}

	return &SpillWriter{
		file: f,
		enc:  msgpack.NewEncoder(f),
	}, nil
}

// NewManagedSpillWriter creates a spill writer whose file is tracked by the
// given SpillManager. If mgr is nil, falls back to NewSpillWriter behavior.
func NewManagedSpillWriter(mgr *SpillManager, prefix string) (*SpillWriter, error) {
	if mgr == nil {
		return NewSpillWriter()
	}

	f, err := mgr.NewSpillFile(prefix)
	if err != nil {
		return nil, fmt.Errorf("spill: create managed temp: %w", err)
	}

	return &SpillWriter{
		file: f,
		enc:  msgpack.NewEncoder(f),
		mgr:  mgr,
	}, nil
}

// spillRow is a serializable row for spilling.
type spillRow struct {
	Fields map[string]spillValue `msgpack:"f"`
}

type spillValue struct {
	Type uint8                 `msgpack:"t"`
	Str  string                `msgpack:"s,omitempty"`
	Num  int64                 `msgpack:"n,omitempty"`
	Flt  float64               `msgpack:"f,omitempty"`
	Arr  []spillValue          `msgpack:"a,omitempty"`
	Obj  map[string]spillValue `msgpack:"o,omitempty"`
}

// Spill wire type tags — stable, append-only. Flt remains the wire field for
// floats even though event.Value stores float bits in the num slot internally.
//
//	0 = null, 1 = string, 2 = int, 3 = float (uses Flt wire field),
//	4 = bool, 5 = timestamp, 6 = duration, 7 = array, 8 = object
const (
	spillTypeNull      uint8 = 0
	spillTypeString    uint8 = 1
	spillTypeInt       uint8 = 2
	spillTypeFloat     uint8 = 3
	spillTypeBool      uint8 = 4
	spillTypeTimestamp uint8 = 5
	spillTypeDuration  uint8 = 6
	spillTypeArray     uint8 = 7
	spillTypeObject    uint8 = 8
)

func toSpillValue(v event.Value) spillValue {
	if v.IsNull() {
		return spillValue{Type: spillTypeNull}
	}
	switch v.Type() {
	case event.FieldTypeString:
		s, _ := v.TryAsString()
		return spillValue{Type: spillTypeString, Str: s}
	case event.FieldTypeInt:
		n, _ := v.TryAsInt()
		return spillValue{Type: spillTypeInt, Num: n}
	case event.FieldTypeFloat:
		f, _ := v.TryAsFloat()
		return spillValue{Type: spillTypeFloat, Flt: f}
	case event.FieldTypeBool:
		b, _ := v.TryAsBool()
		if b {
			return spillValue{Type: spillTypeBool, Num: 1}
		}

		return spillValue{Type: spillTypeBool, Num: 0}
	case event.FieldTypeTimestamp:
		t, _ := v.TryAsTimestamp()
		return spillValue{Type: spillTypeTimestamp, Num: t.UnixNano()}
	case event.FieldTypeDuration:
		d, _ := v.TryAsDuration()
		return spillValue{Type: spillTypeDuration, Num: int64(d)}
	case event.FieldTypeArray:
		elems := v.AsArray()
		arr := make([]spillValue, len(elems))
		for i, e := range elems {
			arr[i] = toSpillValue(e)
		}

		return spillValue{Type: spillTypeArray, Arr: arr}
	case event.FieldTypeObject:
		fields := v.AsObject()
		obj := make(map[string]spillValue, len(fields))
		for k, e := range fields {
			obj[k] = toSpillValue(e)
		}

		return spillValue{Type: spillTypeObject, Obj: obj}
	default:
		return spillValue{Type: spillTypeString, Str: v.String()}
	}
}

func fromSpillValue(sv spillValue) event.Value {
	switch sv.Type {
	case spillTypeNull:
		return event.NullValue()
	case spillTypeString:
		return event.StringValue(sv.Str)
	case spillTypeInt:
		return event.IntValue(sv.Num)
	case spillTypeFloat:
		return event.FloatValue(sv.Flt)
	case spillTypeBool:
		if sv.Num != 0 {
			return event.BoolValue(true)
		}

		return event.BoolValue(false)
	case spillTypeTimestamp:
		return event.TimestampValue(time.Unix(0, sv.Num))
	case spillTypeDuration:
		return event.DurationValue(time.Duration(sv.Num))
	case spillTypeArray:
		elems := make([]event.Value, len(sv.Arr))
		for i, e := range sv.Arr {
			elems[i] = fromSpillValue(e)
		}

		return event.ArrayValue(elems)
	case spillTypeObject:
		fields := make(map[string]event.Value, len(sv.Obj))
		for k, e := range sv.Obj {
			fields[k] = fromSpillValue(e)
		}

		return event.ObjectValue(fields)
	default:
		return event.NullValue()
	}
}

// estimatedSpillRowBytes is the estimated serialized size per spill row for
// quota tracking. Deliberately conservative (over-estimate) to ensure the quota
// is reached before actual disk usage exceeds the limit.
const estimatedSpillRowBytes int64 = 512

// WriteRow writes a single row to the spill file.
// Tracks estimated bytes written against the SpillManager for quota enforcement.
func (sw *SpillWriter) WriteRow(row map[string]event.Value) error {
	sr := spillRow{Fields: make(map[string]spillValue, len(row))}
	for k, v := range row {
		sr.Fields[k] = toSpillValue(v)
	}
	if err := sw.enc.Encode(&sr); err != nil {
		return fmt.Errorf("spill: encode row: %w", err)
	}
	sw.rows++

	// Track estimated bytes written for SpillManager quota enforcement.
	// A fixed estimate is used rather than Seek-based position tracking to
	// avoid overhead on hot paths and interaction with encoder buffering.
	if sw.mgr != nil {
		sw.mgr.TrackBytes(estimatedSpillRowBytes)
	}

	return nil
}

// WriteSorted writes sorted rows and returns the number of rows written.
func (sw *SpillWriter) WriteSorted(rows []map[string]event.Value, sortFields []SortField) error {
	sort.SliceStable(rows, func(i, j int) bool {
		for _, sf := range sortFields {
			a := rows[i][sf.Name]
			b := rows[j][sf.Name]
			cmp := compareSpillValues(a, b)
			if cmp == 0 {
				continue
			}
			if sf.Desc {
				return cmp > 0
			}

			return cmp < 0
		}

		return false
	})
	for _, row := range rows {
		if err := sw.WriteRow(row); err != nil {
			return err
		}
	}

	return nil
}

func compareSpillValues(a, b event.Value) int {
	return vm.CompareValues(a, b)
}

// Close closes the spill file handle.
//
// Behavior depends on whether the writer is managed:
//   - Managed (mgr != nil): Closes the file handle only. The file remains on
//     disk and is tracked by the SpillManager. Call SpillManager.Release(path)
//     to delete it when the data is no longer needed.
//   - Unmanaged (mgr == nil): Closes the file handle AND removes the file from
//     disk. This is the legacy behavior for callers that don't use SpillManager.
//
// For the common pattern of "write, close for reading, read back, then release",
// use CloseFile() instead of Close() — it always preserves the file.
func (sw *SpillWriter) Close() error {
	name := sw.file.Name()
	closeErr := sw.file.Close()

	if sw.mgr == nil {
		if rmErr := os.Remove(name); rmErr != nil && closeErr == nil {
			return rmErr
		}
	}

	return closeErr
}

// CloseFile closes the underlying file handle without removing it.
// Use this when the spill file will be read back later (e.g., sort runs).
func (sw *SpillWriter) CloseFile() error {
	return sw.file.Close()
}

// Path returns the path to the spill file.
func (sw *SpillWriter) Path() string {
	return sw.file.Name()
}

// Rows returns the number of rows written.
func (sw *SpillWriter) Rows() int {
	return sw.rows
}

// SpillReader reads rows back from a spill file.
type SpillReader struct {
	file *os.File
	dec  *msgpack.Decoder
}

// NewSpillReader opens a spill file for reading.
func NewSpillReader(path string) (*SpillReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("spill: open %s: %w", path, err)
	}

	return &SpillReader{
		file: f,
		dec:  msgpack.NewDecoder(f),
	}, nil
}

// ReadRow reads the next row from the spill file.
// Returns nil, io.EOF at end.
func (sr *SpillReader) ReadRow() (map[string]event.Value, error) {
	var row spillRow
	if err := sr.dec.Decode(&row); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}

		return nil, fmt.Errorf("spill: decode row: %w", err)
	}
	result := make(map[string]event.Value, len(row.Fields))
	for k, v := range row.Fields {
		result[k] = fromSpillValue(v)
	}

	return result, nil
}

// Close closes the spill reader.
func (sr *SpillReader) Close() error {
	return sr.file.Close()
}

// mergeEntry holds a row and its source reader index for the merge heap.
type mergeEntry struct {
	row   map[string]event.Value
	index int // which reader produced this row
}

// mergeHeap implements container/heap.Interface for k-way merge.
type mergeHeap struct {
	entries    []mergeEntry
	sortFields []SortField
}

func (h *mergeHeap) Len() int { return len(h.entries) }

func (h *mergeHeap) Less(i, j int) bool {
	a := h.entries[i].row
	b := h.entries[j].row
	for _, sf := range h.sortFields {
		av := a[sf.Name]
		bv := b[sf.Name]
		cmp := vm.CompareValues(av, bv)
		if cmp == 0 {
			continue
		}
		if sf.Desc {
			return cmp > 0
		}

		return cmp < 0
	}
	// Stable tie-breaking by reader index (lower index = earlier data).
	return h.entries[i].index < h.entries[j].index
}

func (h *mergeHeap) Swap(i, j int) {
	h.entries[i], h.entries[j] = h.entries[j], h.entries[i]
}

func (h *mergeHeap) Push(x interface{}) {
	h.entries = append(h.entries, x.(mergeEntry))
}

func (h *mergeHeap) Pop() interface{} {
	old := h.entries
	n := len(old)
	item := old[n-1]
	h.entries = old[:n-1]

	return item
}

// SpillMerger performs k-way merge of sorted spill files using a min-heap.
// This provides O(log k) per-row merge instead of O(k) linear scan.
type SpillMerger struct {
	readers    []*SpillReader
	sortFields []SortField
	h          *mergeHeap
}

// maxMergeFanIn is the maximum number of spill files merged simultaneously.
// If len(paths) exceeds this, a multi-pass cascade is used: groups of
// maxMergeFanIn files are merged into intermediate temp files, then those
// intermediates are merged. This caps open file descriptors at maxMergeFanIn.
const maxMergeFanIn = 64

// NewSpillMerger creates a k-way merge reader over sorted spill files.
// Each file must contain rows sorted according to sortFields.
//
// If len(paths) > maxMergeFanIn, a multi-pass merge is used: paths are split
// into chunks of maxMergeFanIn, each chunk is merged into an intermediate temp
// file, then those intermediate files are merged. This prevents exceeding OS
// file descriptor limits for extreme spill counts.
func NewSpillMerger(paths []string, sortFields []SortField) (*SpillMerger, error) {
	return newSpillMergerWithMgr(paths, sortFields, nil)
}

// NewSpillMergerManaged creates a k-way merge reader with SpillManager support
// for intermediate files in multi-pass merges.
func NewSpillMergerManaged(paths []string, sortFields []SortField, mgr *SpillManager) (*SpillMerger, error) {
	return newSpillMergerWithMgr(paths, sortFields, mgr)
}

func newSpillMergerWithMgr(paths []string, sortFields []SortField, mgr *SpillManager) (*SpillMerger, error) {
	// Multi-pass cascade when fan-in exceeds the limit.
	if len(paths) > maxMergeFanIn {
		reduced, err := cascadeMerge(paths, sortFields, mgr)
		if err != nil {
			return nil, fmt.Errorf("spill: cascade merge: %w", err)
		}
		paths = reduced
	}

	readers := make([]*SpillReader, len(paths))

	for i, path := range paths {
		r, err := NewSpillReader(path)
		if err != nil {
			// Close already opened readers.
			for j := 0; j < i; j++ {
				readers[j].Close()
			}

			return nil, err
		}
		readers[i] = r
	}

	h := &mergeHeap{
		sortFields: sortFields,
	}

	// Prime the heap with the first row from each reader.
	for i, r := range readers {
		row, err := r.ReadRow()
		if err != nil {
			if errors.Is(err, io.EOF) {
				continue
			}

			closeErr := closeSpillReaders(readers)
			return nil, errors.Join(
				fmt.Errorf("spill: prime reader %d: %w", i, err),
				closeErr,
			)
		}
		h.entries = append(h.entries, mergeEntry{row: row, index: i})
	}
	heap.Init(h)

	return &SpillMerger{
		readers:    readers,
		sortFields: sortFields,
		h:          h,
	}, nil
}

// cascadeMerge reduces len(paths) to <= maxMergeFanIn by merging chunks into
// intermediate files. Each chunk of maxMergeFanIn sorted files is merged into
// a single sorted temp file. Returns the paths to the intermediate files.
func cascadeMerge(paths []string, sortFields []SortField, mgr *SpillManager) ([]string, error) {
	for len(paths) > maxMergeFanIn {
		var nextPaths []string
		for i := 0; i < len(paths); i += maxMergeFanIn {
			end := i + maxMergeFanIn
			if end > len(paths) {
				end = len(paths)
			}
			chunk := paths[i:end]

			// If this chunk is small enough, keep it as-is for the final merge.
			if len(chunk) <= 1 {
				nextPaths = append(nextPaths, chunk...)

				continue
			}

			// Merge chunk into an intermediate file.
			sm, err := newSpillMergerWithMgr(chunk, sortFields, nil)
			if err != nil {
				return nil, err
			}

			sw, swErr := NewManagedSpillWriter(mgr, "merge-intermediate")
			if swErr != nil {
				sm.Close()

				return nil, swErr
			}

			for {
				row, mergeErr := sm.Next()
				if mergeErr != nil {
					sm.Close()
					sw.Close()

					return nil, mergeErr
				}
				if row == nil {
					break
				}
				if writeErr := sw.WriteRow(row); writeErr != nil {
					sm.Close()
					sw.Close()

					return nil, writeErr
				}
			}
			sm.Close()

			if closeErr := sw.CloseFile(); closeErr != nil {
				return nil, closeErr
			}
			nextPaths = append(nextPaths, sw.Path())
		}
		paths = nextPaths
	}

	return paths, nil
}

// Next returns the next row in sorted order across all spill files.
// Returns (nil, nil) when all files are exhausted.
func (sm *SpillMerger) Next() (map[string]event.Value, error) {
	if sm.h.Len() == 0 {
		return nil, nil
	}

	entry := heap.Pop(sm.h).(mergeEntry)
	result := entry.row

	// Read the next row from the same reader and push it back.
	if err := sm.refill(entry.index); err != nil {
		return nil, err
	}

	return result, nil
}

// NextBatch returns up to batchSize rows in sorted order.
// Returns (nil, nil) when all files are exhausted (EOF).
func (sm *SpillMerger) NextBatch(batchSize int) (*Batch, error) {
	if sm.h.Len() == 0 {
		return nil, nil
	}

	batch := NewBatch(batchSize)
	for i := 0; i < batchSize; i++ {
		if sm.h.Len() == 0 {
			break
		}
		entry := heap.Pop(sm.h).(mergeEntry)
		batch.AddRow(entry.row)

		// Refill from the same reader.
		if err := sm.refill(entry.index); err != nil {
			return nil, err
		}
	}

	if batch.Len == 0 {
		return nil, nil
	}

	return batch, nil
}

func (sm *SpillMerger) refill(index int) error {
	nextRow, err := sm.readers[index].ReadRow()
	if err == nil {
		heap.Push(sm.h, mergeEntry{row: nextRow, index: index})

		return nil
	}
	if errors.Is(err, io.EOF) {
		return nil
	}

	return fmt.Errorf("spill: refill reader %d: %w", index, err)
}

// Close closes all spill readers.
func (sm *SpillMerger) Close() error {
	return closeSpillReaders(sm.readers)
}

func closeSpillReaders(readers []*SpillReader) error {
	var errs []error
	for _, r := range readers {
		if r == nil {
			continue
		}
		if err := r.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
