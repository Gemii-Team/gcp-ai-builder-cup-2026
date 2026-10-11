package google

import (
	"errors"
	"net/http"

	"google.golang.org/api/googleapi"
)

// IsAccessDenied reports whether err means the user's Google access to Meet is gone:
// a *ConsentRequiredError (never granted, or revoked at token refresh) or a Meet HTTP
// 401/403 (the grant exists but Meet refuses it). Callers fail the work with a
// reason instead of retrying, and need not import googleapi to ask.
func IsAccessDenied(err error) bool {
	var consent *ConsentRequiredError
	if errors.As(err, &consent) {
		return true
	}
	var api *googleapi.Error
	return errors.As(err, &api) && (api.Code == http.StatusUnauthorized || api.Code == http.StatusForbidden)
}
