package ghapi

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/cli/go-gh/v2/pkg/config"
)

const (
	publicAnonymous = "anonymous"
	publicStored    = "stored"
	publicEnv       = "env"
)

// DotcomTokenEnv holds a github.com token for the public fallback of a
// *.ghe.com run, for CI where there is no gh login. A GHE.com job's own
// token is issued by the tenant, so github.com doesn't accept it.
const DotcomTokenEnv = "GH_ACTIONS_LOCK_GITHUB_COM_TOKEN"

// publicCredential picks the public fallback credential: DotcomTokenEnv,
// then the stored github.com login, else none. Under the Dependabot proxy
// it is always none, with auth "": the proxy authenticates the request, and
// its container has no gh login to read.
func publicCredential(skipStored bool) (token, auth string) {
	if DependabotProxy() {
		return "", ""
	}
	if token := os.Getenv(DotcomTokenEnv); token != "" {
		return token, publicEnv
	}
	if !skipStored {
		if token := storedDotcomToken(); token != "" {
			return token, publicStored
		}
	}
	return "", publicAnonymous
}

// storedDotcomToken returns the credential `gh auth login --hostname
// github.com` stored, or "". It is go-gh's auth.TokenForHost("github.com")
// without the GH_TOKEN and GITHUB_TOKEN step: go-gh applies those to
// github.com and *.ghe.com alike, so in a tenant run they hold the tenant
// token, which must not reach github.com.
func storedDotcomToken() string {
	if cfg, err := config.Read(nil); err == nil {
		if token, err := cfg.Get([]string{"hosts", "github.com", "oauth_token"}); err == nil && token != "" {
			return token
		}
	}
	gh := os.Getenv("GH_PATH")
	if gh == "" {
		gh, _ = exec.LookPath("gh")
	}
	if gh == "" {
		return ""
	}
	// --secure-storage reads only the keyring, never token variables.
	out, err := exec.Command(gh, "auth", "token", "--secure-storage", "--hostname", "github.com").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// publicTransport sends github.com public fallback requests with token, or
// with no credentials when token is empty. The token goes only to
// https://api.github.com, so a redirect elsewhere can't carry it.
type publicTransport struct {
	inner http.RoundTripper
	token string
}

func (t publicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Del("Cookie")
	if t.token != "" && req.URL.Scheme == "https" && strings.EqualFold(req.URL.Host, "api.github.com") {
		req.Header.Set("Authorization", "token "+t.token)
	} else {
		req.Header.Del("Authorization")
	}
	return t.inner.RoundTrip(req)
}

// publicError is an error from a public fallback lookup, with how the
// lookup was authenticated.
type publicError struct {
	auth string
	err  error
}

func (e *publicError) Error() string { return e.err.Error() }
func (e *publicError) Unwrap() error { return e.err }

// publicErr records on err how c's public fallback lookups authenticate.
func (c *Client) publicErr(err error) error {
	if c.publicAuth == "" {
		return err
	}
	return &publicError{auth: c.publicAuth, err: err}
}

func publicAuthOf(err error) string {
	var pe *publicError
	if errors.As(err, &pe) {
		return pe.auth
	}
	return ""
}

// AnonymousFallbackLimited reports whether err's cause is a rate limit on
// the anonymous github.com public fallback of a *.ghe.com run. That limit
// is per IP, so no tenant token can raise it.
func AnonymousFallbackLimited(err error) bool {
	if err == nil {
		return false
	}
	c := cause(err)
	return rateLimited(c) && publicAuthOf(c) == publicAnonymous
}

// AnonymousFallbackHint tells the user how to authenticate the github.com
// public fallback.
const AnonymousFallbackHint = "authenticate public github.com lookups, which are otherwise anonymous and limited to 60 requests/hour per IP: " +
	"locally, run `gh auth login --hostname github.com`; in CI, set " + DotcomTokenEnv + " to a github.com token with no permissions"
