package ghapi

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/cli/go-gh/v2/pkg/api"
)

// StatusCode reports the HTTP status code carried by err when it originates
// from a REST call. ok is false when err is nil, a transport failure, or a
// GraphQL error that carries no HTTP status. It lets callers classify failures
// without importing go-gh, keeping the API transport internal to this package.
func StatusCode(err error) (code int, ok bool) {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode, true
	}
	return 0, false
}

// IsNotFound reports whether err is a 404 from the GitHub REST API.
func IsNotFound(err error) bool {
	code, ok := StatusCode(err)
	return ok && code == http.StatusNotFound
}

// IsPermissionDenied reports whether err is a 401, or a 403 that is not a
// rate-limit response. Rate-limit 403s are reported by IsRateLimited instead so
// callers can back off rather than treat the repo as inaccessible.
func IsPermissionDenied(err error) bool {
	code, ok := StatusCode(err)
	if !ok {
		return false
	}
	if code == http.StatusUnauthorized {
		return true
	}
	return code == http.StatusForbidden && !IsRateLimited(err)
}

// IsRateLimited reports whether err is a rate-limit response: HTTP 429, or a
// 403 with the budget exhausted or a Retry-After (secondary rate limit), the
// same rule retryTransport.shouldRetry uses.
func IsRateLimited(err error) bool {
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return httpErr.StatusCode == http.StatusForbidden &&
		(httpErr.Headers.Get("X-RateLimit-Remaining") == "0" || httpErr.Headers.Get("Retry-After") != "")
}

// IsTransient reports whether err is a failure a retry may fix: a rate
// limit, a 5xx, or a transport error. 404s, 422s and permission errors are
// answers, not outages. A joined error is transient when any branch is, so
// one 404 can't mask an outage elsewhere in the batch.
func IsTransient(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if IsTransient(e) {
				return true
			}
		}
		return false
	}
	if IsRateLimited(err) {
		return true
	}
	if code, ok := StatusCode(err); ok {
		return code >= http.StatusInternalServerError
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// RefNotFoundError reports a ref that does not exist in a repository that
// does. Unlike a transport or rate-limit failure, retrying will not help.
type RefNotFoundError struct {
	Owner, Repo, Ref string
	Err              error
}

func (e *RefNotFoundError) Error() string { return e.Err.Error() }
func (e *RefNotFoundError) Unwrap() error { return e.Err }
