// Package errs holds the sentinel errors every domain returns and the API shell
// maps to status codes. Callers wrap them with fmt.Errorf("...: %w", errs.ErrX)
// and test with errors.Is.
package errs

import "errors"

// The domain error sentinels.
var (
	// ErrNotFound means the thing does not exist, or the caller may not know it does.
	ErrNotFound = errors.New("not found")
	// ErrForbidden means the actor is not allowed to do this.
	ErrForbidden = errors.New("forbidden")
	// ErrConflict means the request clashes with the current state, for example a
	// draft that is already confirmed.
	ErrConflict = errors.New("conflict")
	// ErrInvalid means the request itself is malformed or breaks a rule.
	ErrInvalid = errors.New("invalid")
)
