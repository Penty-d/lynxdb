package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/storage/segment"
)

func main() {
	outDir := filepath.Join("testdata", "segments")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fatal(err)
	}

	fixtures := []struct {
		name        string
		compression segment.CompressionType
		sortKey     []string
		events      []*event.Event
	}{
		{name: "v1.lsg", compression: segment.CompressionZSTD, events: fixtureEvents("zstd", 6)},
		{name: "v1_no_caps.lsg", compression: segment.CompressionNone, events: fixtureEvents("plain", 3)},
		{name: "v1_with_inverted.lsg", compression: segment.CompressionLZ4, events: fixtureEvents("search", 4)},
		{name: "v1_with_primary.lsg", compression: segment.CompressionLZ4, sortKey: []string{"host"}, events: fixtureEvents("primary", 4)},
	}

	for _, spec := range fixtures {
		var buf bytes.Buffer
		w := segment.NewWriterWithCompression(&buf, spec.compression)
		if len(spec.sortKey) > 0 {
			w.SetSortKey(spec.sortKey)
		}
		if _, err := w.Write(spec.events); err != nil {
			fatal(fmt.Errorf("%s: %w", spec.name, err))
		}
		if err := os.WriteFile(filepath.Join(outDir, spec.name), buf.Bytes(), 0o644); err != nil {
			fatal(err)
		}
	}
}

func fixtureEvents(prefix string, n int) []*event.Event {
	base := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	events := make([]*event.Event, 0, n)
	for i := 0; i < n; i++ {
		e := event.NewEvent(base.Add(time.Duration(i)*time.Second), fmt.Sprintf("%s event %02d level=info token=alpha", prefix, i))
		e.Source = "fixture.log"
		e.SourceType = "fixture"
		e.Host = fmt.Sprintf("host-%02d", i)
		e.Index = "main"
		e.SetField("level", event.StringValue("info"))
		e.SetField("status", event.IntValue(200+int64(i%3)))
		e.SetField("latency", event.FloatValue(float64(i)+0.5))
		events = append(events, e)
	}
	return events
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
