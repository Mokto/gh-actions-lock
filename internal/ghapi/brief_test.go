package ghapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/stretchr/testify/assert"
)

func TestBrief(t *testing.T) {
	requestURL, err := url.Parse("https://api.github.com/repos/acme/widget/commits/v1")
	assert.NoError(t, err)
	const saml = "Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "ordinary HTTP failure omits message and request URL",
			err:  &api.HTTPError{StatusCode: http.StatusServiceUnavailable, RequestURL: requestURL, Message: "upstream unavailable"},
			want: "GitHub returned 503 Service Unavailable",
		},
		{
			name: "ordinary forbidden failure remains concise",
			err:  &api.HTTPError{StatusCode: http.StatusForbidden, RequestURL: requestURL, Message: "Resource not accessible by integration"},
			want: "GitHub returned 403 Forbidden",
		},
		{
			name: "wrapped SAML failure retains authorization instructions",
			err:  fmt.Errorf("fetching commit: %w", &api.HTTPError{StatusCode: http.StatusForbidden, RequestURL: requestURL, Message: saml}),
			want: "GitHub returned 403 Forbidden: " + saml,
		},
		{
			name: "SAML SSO failure retains authorization instructions",
			err:  &api.HTTPError{StatusCode: http.StatusForbidden, RequestURL: requestURL, Message: "SAML SSO required. Authorize your token and retry."},
			want: "GitHub returned 403 Forbidden: SAML SSO required. Authorize your token and retry.",
		},
		{
			name: "wrapped rate-limited SSO fallback retains authorization URL",
			err: fmt.Errorf("fetching commit: %w", &SSORateLimitedError{
				Host: "github.com", Owner: "acme",
				Err: &api.HTTPError{StatusCode: http.StatusTooManyRequests, RequestURL: requestURL},
			}),
			want: ssoRequiredMessage("github.com", "acme") + " (the anonymous fallback for public repositories was rate-limited)",
		},
		{
			name: "GraphQL SSO guidance is unchanged",
			err:  errors.New(ssoRequiredMessage("github.com", "acme")),
			want: ssoRequiredMessage("github.com", "acme"),
		},
		{
			name: "non-HTTP error is unchanged",
			err:  errors.New("connection refused"),
			want: "connection refused",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := &BriefError{Msg: Brief(tt.err), Err: tt.err}
			assert.Equal(t, tt.want, wrapped.Error())
			assert.NotContains(t, wrapped.Error(), requestURL.String())
			assert.ErrorIs(t, wrapped, tt.err)
			assert.Equal(t, tt.err.Error(), wrapped.Verbose())
			if code, ok := StatusCode(tt.err); ok {
				got, found := StatusCode(wrapped)
				assert.True(t, found)
				assert.Equal(t, code, got)
			}
		})
	}
}
