package pipeline

import (
	"testing"
)

func TestMakeresultsRowsFromCSV(t *testing.T) {
	rows, err := makeresultsRowsFromData("csv", "host,status\nweb-01,200\nweb-02,500\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(rows))
	}
	if rows[0]["host"].String() != "web-01" || rows[0]["status"].String() != "200" {
		t.Errorf("first row mismatch: %+v", rows[0])
	}
	if rows[1]["host"].String() != "web-02" {
		t.Errorf("second row mismatch: %+v", rows[1])
	}
	for i, row := range rows {
		if _, ok := row["_time"]; !ok {
			t.Errorf("row %d: missing default _time", i)
		}
	}
}

func TestMakeresultsRowsFromJSON(t *testing.T) {
	rows, err := makeresultsRowsFromData("json", `[{"host":"web-01","status":200},{"host":"web-02","status":500}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(rows))
	}
	if rows[0]["host"].String() != "web-01" {
		t.Errorf("first row mismatch: %+v", rows[0])
	}
	if rows[1]["status"].AsFloat() != 500 {
		t.Errorf("json numbers should decode as floats, got %+v", rows[1]["status"])
	}
}

func TestMakeresultsRowsFromDataErrors(t *testing.T) {
	if _, err := makeresultsRowsFromData("json", `{"not":"an array"}`); err == nil {
		t.Error("expected error for non-array json data")
	}
	if _, err := makeresultsRowsFromData("csv", ""); err == nil {
		t.Error("expected error for empty csv data")
	}
	if _, err := makeresultsRowsFromData("xml", "x"); err == nil {
		t.Error("expected error for unsupported format")
	}
}
