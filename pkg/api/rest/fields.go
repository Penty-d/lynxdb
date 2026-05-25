package rest

import (
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/lynxbase/lynxdb/pkg/usecases"
)

func (s *Server) handleFieldValues(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	fieldName, ok := requirePathValue(r, w, "name")
	if !ok {
		return
	}

	limit := parseIntParam(r, "limit", 10)
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	result, err := s.queryService.FieldValues(r.Context(), usecases.FieldValuesRequest{
		FieldName: fieldName,
		Limit:     limit,
		From:      firstNonEmpty(r.URL.Query().Get("from"), r.URL.Query().Get("since")),
		To:        r.URL.Query().Get("to"),
	})
	if err != nil {
		respondInternalError(w, err.Error())

		return
	}

	values := make([]map[string]interface{}, len(result.Values))
	for i, fv := range result.Values {
		values[i] = map[string]interface{}{
			"value":   fv.Value,
			"count":   fv.Count,
			"percent": fv.Percent,
		}
	}

	took := time.Since(start)
	respondData(w, http.StatusOK, map[string]interface{}{
		"field":        result.Field,
		"values":       values,
		"unique_count": result.UniqueCount,
		"total_count":  result.TotalCount,
	}, WithTook(took))
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	pattern := r.URL.Query().Get("pattern")

	result, err := s.queryService.ListSources(r.Context())
	if err != nil {
		respondInternalError(w, err.Error())

		return
	}

	sources := make([]map[string]interface{}, 0, len(result.Sources))
	for _, info := range result.Sources {
		if pattern != "" {
			matched, _ := sourceGlobMatch(pattern, info.Name)
			if !matched {
				continue
			}
		}
		sources = append(sources, map[string]interface{}{
			"name":        info.Name,
			"event_count": info.EventCount,
			"first_event": info.FirstEvent.UTC().Format(time.RFC3339),
			"last_event":  info.LastEvent.UTC().Format(time.RFC3339),
		})
	}

	resp := map[string]interface{}{
		"sources": sources,
	}
	if pattern != "" {
		resp["pattern"] = pattern
	}

	took := time.Since(start)
	respondData(w, http.StatusOK, resp, WithTook(took))
}

// sourceGlobMatch wraps path.Match for source name glob matching.
// Supports * (any sequence) and ? (any single char).
func sourceGlobMatch(pattern, name string) (bool, error) {
	return path.Match(pattern, name)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}
