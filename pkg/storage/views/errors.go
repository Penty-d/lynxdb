package views

import "errors"

var (
	ErrViewNotFound      = errors.New("views: not found")
	ErrViewAlreadyExists = errors.New("views: already exists")
	ErrViewNameInvalid   = errors.New("views: invalid name")
	ErrViewNameEmpty     = errors.New("views: name must not be empty")
	ErrInvalidRetention  = errors.New("views: retention must be non-negative")
	ErrNoColumns         = errors.New("views: columns must not be empty")

	// ErrViewNeedsMigration is returned when a query is executed against a
	// view whose status is needs-migration. The view's SPL2 query failed to
	// parse at startup and must be translated to LynxFlow before it can serve
	// queries. Callers should suggest `lynxdb mv migrate <name>`.
	ErrViewNeedsMigration = errors.New("views: view needs migration to LynxFlow; run `lynxdb mv migrate <name>` to translate the query")
)
