package ghapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

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
// same rule the retry transport uses.
func IsRateLimited(err error) bool {
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	if httpErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return rateLimited403(httpErr.StatusCode, httpErr.Headers)
}

// IsTransient reports whether err is a failure a retry may fix: a rate
// limit, a 5xx, or a transport error other than an untrusted certificate.
// 401s, 404s, 422s and permission errors are answers, not outages. A joined
// error is transient when any branch is, so one 404 can't mask an outage
// elsewhere in the batch. It follows the rules the retry transport uses.
func IsTransient(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if IsTransient(e) {
				return true
			}
		}
		return false
	}
	if IsRateLimited(err) || graphQLRateLimited(err) || errors.As(err, new(*UnknownOutcomeError)) {
		return true
	}
	if code, ok := StatusCode(err); ok {
		return code >= http.StatusInternalServerError
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr) && retryableTransportErr(urlErr.Err)
}

// ConfigError returns a one-line error when err shows the run itself is
// misconfigured: GitHub's TLS certificate isn't trusted (a proxy or CA
// bundle), or GitHub rejected the token. Every request would fail the same
// way, so it is reported once rather than per pin. It returns nil otherwise.
func ConfigError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && untrustedCert(urlErr.Err) {
		host := urlErr.URL
		if u, perr := url.Parse(urlErr.URL); perr == nil && u.Host != "" {
			host = u.Host
		}
		return &BriefError{Msg: fmt.Sprintf("TLS certificate for %s is not trusted; check HTTPS_PROXY or your CA bundle", host), Err: err}
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusUnauthorized {
		login := "gh auth login"
		if httpErr.RequestURL != nil {
			if host := strings.TrimPrefix(httpErr.RequestURL.Hostname(), "api."); host != "github.com" {
				login += " --hostname " + host
			}
		}
		return &BriefError{Msg: fmt.Sprintf("GitHub rejected the token (401); run `%s`", login), Err: err}
	}
	return nil
}

// graphQLRateLimited reports a GraphQL response rejected by the rate limit.
// GitHub sends it as HTTP 200 with a RATE_LIMITED error.
func graphQLRateLimited(err error) bool {
	var gqlErr *api.GraphQLError
	if !errors.As(err, &gqlErr) {
		return false
	}
	for _, e := range gqlErr.Errors {
		if e.Type == "RATE_LIMITED" {
			return true
		}
	}
	return false
}

// RefNotFoundError reports a ref that does not exist in a repository that
// does. Unlike a transport or rate-limit failure, retrying will not help.
type RefNotFoundError struct {
	Owner, Repo, Ref string
	Err              error
}

func (e *RefNotFoundError) Error() string { return e.Err.Error() }
func (e *RefNotFoundError) Unwrap() error { return e.Err }

// errHeaders returns the response headers carried by a REST error.
func errHeaders(err error) (http.Header, bool) {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Headers != nil {
		return httpErr.Headers, true
	}
	return nil, false
}
