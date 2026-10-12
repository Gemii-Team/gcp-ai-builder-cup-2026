package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

// The reasons of a MeetError, in the vocabulary the meetings pipeline fails a meeting
// with.
const (
	// ReasonMeetAccess: Meet no longer accepts the user's grant.
	ReasonMeetAccess = "meet_access"
	// ReasonMeetUnavailable: Meet cannot do what was asked, for the cause the MeetError names.
	ReasonMeetUnavailable = "meet_unavailable"
)

// The causes of a MeetError.
const (
	// CauseAccessDenied (ReasonMeetAccess): Meet answered 401, or 403 for a missing scope.
	CauseAccessDenied = "access_denied"
	// CauseAPIDisabled: the Meet API is not enabled in the Google project; only an operator can fix it.
	CauseAPIDisabled = "api_disabled"
	// CauseRateLimited: Meet answered 429.
	CauseRateLimited = "rate_limited"
	// CauseUnavailable: Meet answered 5xx, or the request never got an answer (network, timeout).
	CauseUnavailable = "unavailable"
	// CauseRefused: Meet refused the request for any other 4xx reason.
	CauseRefused = "refused"
)

// MeetError is a failure of Meet that a person can be told about, sorted into a Reason
// and a Cause. Its text is fixed: it never carries Google's own message, which can
// name the project, the account or the request. The client returns it inside a chain
// that still holds the original error, so the operator's log keeps the full text and
// errors.As still finds the *googleapi.Error.
type MeetError struct {
	Reason string
	Cause  string
	// Status is the HTTP status Meet answered with, 0 when it never answered.
	Status int
}

// Error implements error.
func (e *MeetError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("google: Meet failed (%s, %s)", e.Reason, e.Cause)
	}
	return fmt.Sprintf("google: Meet failed (%s, %s, HTTP %d)", e.Reason, e.Cause, e.Status)
}

// meetFailure is err with its MeetError beside it: the text is err's, whole, for the
// log, and errors.As finds both the MeetError and whatever err wraps.
type meetFailure struct {
	err  error
	meet *MeetError
}

func (f *meetFailure) Error() string   { return f.err.Error() }
func (f *meetFailure) Unwrap() []error { return []error{f.err, f.meet} }

// classify sorts err into a MeetError, or returns nil when err is not a failure of
// Meet: a cancelled request, a response nobody can read, anything of the application's
// own. Only what came back from the HTTP call counts: an answer from Meet (a
// *googleapi.Error), an answer from Google's token endpoint refusing the OAuth client
// (a *oauth2.RetrieveError, which the http.Client also wraps in a *url.Error because
// the request failed before it left) or no answer at all (every transport error,
// timeouts included, in a *url.Error). A token endpoint that is limiting (429) or failing
// (5xx) is told as Meet's own would be; any other refusal of it (invalid_client and
// unauthorized_client mean the operator's credentials are wrong or disabled) is a
// refusal, the cause that asks the person to tell whoever runs K.I.W.I. The token
// endpoint is not Meet, so none of these has a Meet status.
func classify(err error) *MeetError {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	var api *googleapi.Error
	if errors.As(err, &api) {
		return classifyStatus(api)
	}
	var token *oauth2.RetrieveError
	if errors.As(err, &token) {
		me := &MeetError{Reason: ReasonMeetUnavailable, Cause: CauseRefused}
		switch {
		case token.Response == nil:
		case token.Response.StatusCode == http.StatusTooManyRequests:
			me.Cause = CauseRateLimited
		case token.Response.StatusCode >= http.StatusInternalServerError:
			me.Cause = CauseUnavailable
		}
		return me
	}
	var transport *url.Error
	if errors.As(err, &transport) {
		return &MeetError{Reason: ReasonMeetUnavailable, Cause: CauseUnavailable}
	}
	return nil
}

func classifyStatus(api *googleapi.Error) *MeetError {
	me := &MeetError{Reason: ReasonMeetUnavailable, Status: api.Code}
	switch {
	case api.Code == http.StatusUnauthorized,
		api.Code == http.StatusForbidden && hasReason(api, "ACCESS_TOKEN_SCOPE_INSUFFICIENT"):
		me.Reason, me.Cause = ReasonMeetAccess, CauseAccessDenied
	case api.Code == http.StatusForbidden && (hasReason(api, "SERVICE_DISABLED") || hasReason(api, "accessNotConfigured")):
		me.Cause = CauseAPIDisabled
	case api.Code == http.StatusTooManyRequests:
		me.Cause = CauseRateLimited
	case api.Code >= http.StatusInternalServerError:
		me.Cause = CauseUnavailable
	default:
		me.Cause = CauseRefused
	}
	return me
}

// errorInfoType is the @type of the google.rpc.ErrorInfo detail Google adds to an error.
const errorInfoType = "type.googleapis.com/google.rpc.ErrorInfo"

// errorInfo is a google.rpc.ErrorInfo detail of an error.
type errorInfo struct {
	reason   string
	metadata map[string]any
}

// errorInfos are the ErrorInfo details of api.
func errorInfos(api *googleapi.Error) (infos []errorInfo) {
	for _, d := range api.Details {
		m, ok := d.(map[string]any)
		if !ok || m["@type"] != errorInfoType {
			continue
		}
		reason, _ := m["reason"].(string)
		metadata, _ := m["metadata"].(map[string]any)
		infos = append(infos, errorInfo{reason: reason, metadata: metadata})
	}
	return infos
}

// hasReason reports whether api names reason, as an ErrorInfo detail or in the older
// errors list.
func hasReason(api *googleapi.Error, reason string) bool {
	for _, info := range errorInfos(api) {
		if info.reason == reason {
			return true
		}
	}
	for _, item := range api.Errors {
		if item.Reason == reason {
			return true
		}
	}
	return false
}

// autoTranscriptionRefused reports whether err is Meet telling this user that
// auto-transcription is not available to them: 403 with an ErrorInfo whose reason is
// FEATURE_UNAVAILABLE_TO_USER for the updateAutoTranscriptionGeneration feature. A
// personal Google account answers this; it is the only failure that makes the client
// create the space without the setting.
func autoTranscriptionRefused(err error) bool {
	var api *googleapi.Error
	if !errors.As(err, &api) || api.Code != http.StatusForbidden {
		return false
	}
	for _, info := range errorInfos(api) {
		if info.reason == "FEATURE_UNAVAILABLE_TO_USER" && info.metadata["feature_name"] == "updateAutoTranscriptionGeneration" {
			return true
		}
	}
	return false
}
