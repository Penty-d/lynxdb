package views

import (
	"regexp"
	"time"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/event"
)

// ViewType classifies the kind of materialized view.
type ViewType uint8

const (
	ViewTypeProjection  ViewType = 1
	ViewTypeAggregation ViewType = 2
)

// ColumnDef describes a single column in a materialized view.
type ColumnDef struct {
	Name     string          `json:"name"`
	Type     event.FieldType `json:"type"`
	Encoding string          `json:"encoding"` // "dictionary", "delta", "gorilla", "lz4"
}

// AggregationDef describes an aggregation in an aggregation-type view.
type AggregationDef struct {
	Name         string   `json:"name"`          // e.g. "count", "avg(duration)"
	Type         string   `json:"type"`          // "count", "sum", "avg", "min", "max"
	StateColumns []string `json:"state_columns"` // columns storing partial state
}

// ViewStatus represents the lifecycle state of a materialized view.
type ViewStatus string

const (
	ViewStatusCreating       ViewStatus = "creating"
	ViewStatusBackfill       ViewStatus = "backfill"
	ViewStatusActive         ViewStatus = "active"
	ViewStatusPaused         ViewStatus = "paused"
	ViewStatusRebuilding     ViewStatus = "rebuilding"
	ViewStatusDropping       ViewStatus = "dropping"
	ViewStatusNeedsMigration ViewStatus = "needs-migration"
)

// ViewDefinition is the persistent metadata for a materialized view.
type ViewDefinition struct {
	Name         string           `json:"name"`
	Version      int              `json:"version"`
	Type         ViewType         `json:"type"`
	Query        string           `json:"query"`  // original query text (SPL2 or LynxFlow)
	Filter       string           `json:"filter"` // WHERE clause (e.g. "level=error")
	Columns      []ColumnDef      `json:"columns"`
	Aggregations []AggregationDef `json:"aggregations,omitempty"`
	GroupBy      []string         `json:"group_by,omitempty"`
	SortKey      []string         `json:"sort_key"`
	Retention    time.Duration    `json:"retention"` // 0 = unlimited
	Status       ViewStatus       `json:"status"`
	Cursor       string           `json:"cursor"` // "seg-004:offset-847291"
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`

	// SourceIndex is the FROM clause index name extracted from the query
	// (e.g., "main", "nginx"). Used by the dispatcher to filter events by
	// source index before applying the view's streaming pipeline.
	SourceIndex string `json:"source_index,omitempty"`

	// AggSpec describes the partial aggregation to compute at insert time.
	// Persisted so that deserialization of state columns is possible after
	// server restart. Nil for projection views.
	AggSpec *pipeline.PartialAggSpec `json:"agg_spec,omitempty"`

	// LanguageVersion identifies the query language used in Query.
	// Values: "spl2" or "lynxflow". Empty string is treated as "spl2"
	// for backward compatibility with definitions persisted before this
	// field existed (JSON-additive: old files load unchanged).
	LanguageVersion string `json:"language_version,omitempty"`

	// MigratedFrom holds the original query text before migration from
	// SPL2 to LynxFlow (via `lynxdb mv migrate`). Empty when the view
	// was created directly in the target language.
	MigratedFrom string `json:"migrated_from,omitempty"`
}

// EffectiveLanguage returns the language version for this view's query.
// Empty/absent LanguageVersion is treated as "spl2" for backward compatibility.
func (d *ViewDefinition) EffectiveLanguage() string {
	if d.LanguageVersion == "" {
		return "spl2"
	}
	return d.LanguageVersion
}

// validName matches alphanumeric characters, underscores, and hyphens.
var validName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Validate checks that the view definition has valid fields.
func (d *ViewDefinition) Validate() error {
	if d.Name == "" {
		return ErrViewNameEmpty
	}
	if !validName.MatchString(d.Name) {
		return ErrViewNameInvalid
	}
	if len(d.Columns) == 0 {
		return ErrNoColumns
	}
	if d.Retention < 0 {
		return ErrInvalidRetention
	}

	return nil
}
