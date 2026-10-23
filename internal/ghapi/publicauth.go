package ghapi

import (
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/cli/go-gh/v2/pkg/config"
)

// publicAuthHeader marks responses to the github.com public fallback of a
// *.ghe.com run with how the request was authenticated, so an error can say
// whether it hit the anonymous per-IP rate limit. It never leaves the process.
const publicAuthHeader = "X-Gh-Actions-Lock-Public-Auth"

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
// then the stored github.com login, else anonymous.
func publicCredential(stored func() string) (token, mode string) {
	if token := os.Getenv(DotcomTokenEnv); token != "" {
		return token, publicEnv
	}
	if stored != nil {
		if token := stored(); token != "" {
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
// with no credentials when token is empty, and marks each response with
// publicAuthHeader. Under the Dependabot proxy, which supplies its own
// credential, responses are left unmarked so its messages don't change.
type publicTransport struct {
	inner   http.RoundTripper
	token   string
	mode    string
	proxied bool
}

func (t publicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Del("Cookie")
	mode := t.mode
	if t.token != "" {
		req.Header.Set("Authorization", "token "+t.token)
	} else {
		mode = publicAnonymous
		req.Header.Del("Authorization")
	}
	resp, err := t.inner.RoundTrip(req)
	if resp != nil && !t.proxied {
		if resp.Header == nil {
			resp.Header = http.Header{}
		}
		resp.Header.Set(publicAuthHeader, mode)
	}
	return resp, err
}

func publicAuth(err error) string {
	h, ok := errHeaders(err)
	if !ok {
		return ""
	}
	return h.Get(publicAuthHeader)
}

// AnonymousFallbackLimited reports whether err's cause is a rate limit on
// the anonymous github.com public fallback of a *.ghe.com run. That limit
// is per IP, so no tenant token can raise it.
func AnonymousFallbackLimited(err error) bool {
	if err == nil {
		return false
	}
	c := cause(err)
	return rateLimited(c) && publicAuth(c) == publicAnonymous
}

// AnonymousFallbackHint tells the user how to authenticate the github.com
// public fallback.
const AnonymousFallbackHint = "authenticate public github.com lookups, which are otherwise anonymous and limited to 60 requests/hour per IP: " +
	"locally, run `gh auth login --hostname github.com`; in CI, set " + DotcomTokenEnv + " to a github.com token with no permissions"
