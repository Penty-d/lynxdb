package segment

import (
	"math/bits"
	"sort"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/storage/segment/index"
)

// IndexConfig configures optional segment indexes selected at write time.
// BSIMaxBitCount is a write-time safety cap only; readers accept whatever is
// present on disk. A zero cap disables the cap when passed through SetIndexConfig.
type IndexConfig struct {
	DisableBSI       bool
	ProfileOverrides map[string]IndexProfile
	BSIMaxBitCount   int
}

// RangeBSICandidate describes a catalog column selected for Range BSI.
type RangeBSICandidate struct {
	Name      string
	ValueKind uint8
}

func defaultIndexConfig() IndexConfig {
	return IndexConfig{BSIMaxBitCount: 64}
}

// collectRangeBSIColumns selects columns that should get Range BSI for this
// segment. EqInverted and None remain future profiles in this phase; automatic
// low-cardinality numeric candidates stay on the default index path.
func collectRangeBSIColumns(events []*event.Event, catalog []CatalogEntry, cfg IndexConfig) []RangeBSICandidate {
	if cfg.DisableBSI {
		return nil
	}

	candidates := make([]RangeBSICandidate, 0, len(catalog))
	for _, cat := range catalog {
		if override, ok := cfg.ProfileOverrides[cat.Name]; ok {
			if override == IndexProfileRangeBSI {
				if kind, ok := inferRangeBSIValueKind(events, cat.Name); ok {
					candidates = append(candidates, RangeBSICandidate{Name: cat.Name, ValueKind: kind})
				}
			}
			continue
		}

		switch cat.Name {
		case "_time":
			candidates = append(candidates, RangeBSICandidate{
				Name:      cat.Name,
				ValueKind: index.RangeBSIValueTimestampNS,
			})
		case "_raw", "_source", "_sourcetype", "host", "index":
			continue
		default:
			if cand, ok := autoRangeBSICandidate(events, cat.Name); ok {
				candidates = append(candidates, cand)
			}
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Name < candidates[j].Name
	})
	return candidates
}

func markRangeBSICatalog(catalog []CatalogEntry, candidates []RangeBSICandidate) {
	if len(candidates) == 0 {
		return
	}
	selected := make(map[string]struct{}, len(candidates))
	for _, cand := range candidates {
		selected[cand.Name] = struct{}{}
	}
	for i := range catalog {
		if _, ok := selected[catalog[i].Name]; ok {
			catalog[i].IndexProfile = IndexProfileRangeBSI
		}
	}
}

func inferRangeBSIValueKind(events []*event.Event, name string) (uint8, bool) {
	if name == "_time" {
		return index.RangeBSIValueTimestampNS, true
	}

	typ := event.FieldTypeNull
	for _, e := range events {
		v := e.GetField(name)
		if v.IsNull() {
			continue
		}
		next := v.Type()
		switch next {
		case event.FieldTypeInt, event.FieldTypeFloat, event.FieldTypeTimestamp, event.FieldTypeBool:
		default:
			return 0, false
		}
		if typ == event.FieldTypeNull {
			typ = next
			continue
		}
		if typ == next {
			continue
		}
		if (typ == event.FieldTypeInt && next == event.FieldTypeFloat) ||
			(typ == event.FieldTypeFloat && next == event.FieldTypeInt) {
			typ = event.FieldTypeFloat
			continue
		}
		return 0, false
	}

	switch typ {
	case event.FieldTypeInt:
		return index.RangeBSIValueInt, true
	case event.FieldTypeFloat:
		return index.RangeBSIValueFloat64Bits, true
	case event.FieldTypeTimestamp:
		return index.RangeBSIValueTimestampNS, true
	case event.FieldTypeBool:
		return index.RangeBSIValueBool, true
	default:
		return 0, false
	}
}

func autoRangeBSICandidate(events []*event.Event, name string) (RangeBSICandidate, bool) {
	kind, ok := inferRangeBSIValueKind(events, name)
	if !ok {
		return RangeBSICandidate{}, false
	}
	switch kind {
	case index.RangeBSIValueFloat64Bits:
		minV, maxV, _, ok := scanColumnStats(events, name, kind)
		return RangeBSICandidate{Name: name, ValueKind: kind}, ok && minV != maxV
	case index.RangeBSIValueInt, index.RangeBSIValueTimestampNS:
		minV, maxV, distinct, ok := scanColumnStats(events, name, kind)
		return RangeBSICandidate{Name: name, ValueKind: kind}, ok &&
			distinct > 256 && unsignedSpread(minV, maxV) > 256
	default:
		return RangeBSICandidate{}, false
	}
}

func scanColumnStats(events []*event.Event, name string, kind uint8) (int64, int64, int, bool) {
	var minV, maxV int64
	found := false
	distinct := make(map[int64]struct{})
	for _, e := range events {
		raw, ok := rawRangeBSIValue(e, name, kind)
		if !ok {
			continue
		}
		if !found {
			minV, maxV, found = raw, raw, true
		} else {
			if raw < minV {
				minV = raw
			}
			if raw > maxV {
				maxV = raw
			}
		}
		distinct[raw] = struct{}{}
	}
	return minV, maxV, len(distinct), found
}

func rawRangeBSIValue(e *event.Event, name string, kind uint8) (int64, bool) {
	switch name {
	case "_time":
		return e.Time.UnixNano(), true
	}

	v := e.GetField(name)
	if v.IsNull() {
		return 0, false
	}
	switch kind {
	case index.RangeBSIValueInt:
		n, ok := v.TryAsInt()
		return n, ok
	case index.RangeBSIValueFloat64Bits:
		if n, ok := v.TryAsInt(); ok {
			return index.FloatToOrderedInt64(float64(n)), true
		}
		f, ok := v.TryAsFloat()
		return index.FloatToOrderedInt64(f), ok
	case index.RangeBSIValueTimestampNS:
		t, ok := v.TryAsTimestamp()
		if !ok {
			return 0, false
		}
		return t.UnixNano(), true
	case index.RangeBSIValueBool:
		b, ok := v.TryAsBool()
		return index.BoolToInt64(b), ok
	default:
		return 0, false
	}
}

func unsignedSpread(minV, maxV int64) uint64 {
	return uint64(maxV) - uint64(minV)
}

func spreadFitsInt64(minV, maxV int64) bool {
	const maxInt64 = uint64(1<<63 - 1)
	return unsignedSpread(minV, maxV) <= maxInt64
}

func bitCountForSpread(minV, maxV int64) int {
	spread := unsignedSpread(minV, maxV)
	if spread == 0 {
		return 0
	}
	return bits.Len64(spread)
}
