package pipeline

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
)

// makeresultsRowsFromData builds rows from makeresults inline data.
// format=csv expects a header row followed by records; format=json expects an
// array of objects. Rows without a _time field get the current time.
func makeresultsRowsFromData(format, data string) ([]map[string]event.Value, error) {
	now := time.Now()

	switch format {
	case "csv":
		records, err := csv.NewReader(strings.NewReader(data)).ReadAll()
		if err != nil {
			return nil, fmt.Errorf("makeresults: parse csv data: %w", err)
		}
		if len(records) == 0 {
			return nil, fmt.Errorf("makeresults: csv data requires a header row")
		}
		header := records[0]
		rows := make([]map[string]event.Value, 0, len(records)-1)
		for _, rec := range records[1:] {
			row := make(map[string]event.Value, len(header)+1)
			for i, col := range header {
				if i < len(rec) {
					row[col] = event.StringValue(rec[i])
				}
			}
			if _, ok := row["_time"]; !ok {
				row["_time"] = event.TimestampValue(now)
			}
			rows = append(rows, row)
		}

		return rows, nil

	case "json":
		var objs []map[string]interface{}
		if err := json.Unmarshal([]byte(data), &objs); err != nil {
			return nil, fmt.Errorf("makeresults: parse json data (expected an array of objects): %w", err)
		}
		rows := make([]map[string]event.Value, 0, len(objs))
		for _, obj := range objs {
			row := make(map[string]event.Value, len(obj)+1)
			for k, v := range obj {
				row[k] = event.ValueFromInterface(v)
			}
			if _, ok := row["_time"]; !ok {
				row["_time"] = event.TimestampValue(now)
			}
			rows = append(rows, row)
		}

		return rows, nil

	default:
		return nil, fmt.Errorf("makeresults: unsupported format %q (want csv or json)", format)
	}
}
