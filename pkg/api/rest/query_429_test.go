package rest

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/server"
	"github.com/lynxbase/lynxdb/pkg/usecases"
)

// TestHandlePlanError_TooManyQueriesSetsRetryAfter verifies that the query
// concurrency-limit error maps to HTTP 429 and carries a Retry-After hint so
// clients back off instead of hammering the endpoint.
func TestHandlePlanError_TooManyQueriesSetsRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()

	// usecases.ErrTooManyQueries wraps server.ErrTooManyQueries; handlePlanError
	// matches it via errors.Is.
	handlePlanError(rec, usecases.ErrTooManyQueries)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
}

// TestHandlePlanError_TooManyQueries_ServerSentinel guards that the server-level
// sentinel is also recognised (the usecases sentinel and the server sentinel
// must stay errors.Is-compatible for this mapping to hold).
func TestHandlePlanError_TooManyQueries_ServerSentinel(t *testing.T) {
	rec := httptest.NewRecorder()

	handlePlanError(rec, server.ErrTooManyQueries)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want %q", got, "1")
	}
}
