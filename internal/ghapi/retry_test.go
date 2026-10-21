package ghapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)

// fakeBudget is a run budget on a fake clock: sleeps advance it, and jitter
// always picks its upper bound so waits are deterministic.
func fakeBudget() (*retryBudget, *time.Duration) {
	b := newRetryBudget()
	b.policy = patientPolicy
	var slept time.Duration
	b.now = func() time.Time { return testNow.Add(slept) }
	b.sleep = func(_ context.Context, d time.Duration) { slept += d }
	b.jitter = func(max time.Duration) time.Duration { return max }
	b.notify = func(string) {}
	return b, &slept
}

// backoffTotal is the wait before giving up on a request that never gets
// an answer, with jitter at its upper bound: 1+2+4+8+16+30+30s.
const backoffTotal = 91 * time.Second

func status(code int, h map[string]string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		header := http.Header{}
		for k, v := range h {
			header.Set(k, v)
		}
		return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(bytes.NewReader(nil))}, nil
	}
}

func graphQLBody(body string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader([]byte(body)))}, nil
	}
}

// graphQLRateLimitedResp is GitHub's GraphQL rate-limit rejection: HTTP 200
// with a RATE_LIMITED error.
func graphQLRateLimitedResp(h map[string]string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		resp, _ := graphQLBody(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)()
		for k, v := range h {
			resp.Header.Set(k, v)
		}
		return resp, nil
	}
}

func fail(err error) func() (*http.Response, error) {
	return func() (*http.Response, error) { return nil, err }
}

// TestRetryTransport_Taxonomy pins down, for each failure class, how often
// a request is tried, how long the run waits, and whether the caller gets
// an answer or an unknown outcome.
func TestRetryTransport_Taxonomy(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	tests := []struct {
		name     string
		url      string
		attempt  func() (*http.Response, error)
		attempts int
		waited   time.Duration
		// unknown is true when the caller gets an UnknownOutcomeError,
		// false when it gets an error that is an answer; zero code means
		// an error is expected.
		unknown bool
		code    int
	}{
		{name: "dial refused", attempt: fail(refused), attempts: 8, waited: backoffTotal, unknown: true},
		{name: "dial timeout", attempt: fail(&net.OpError{Op: "dial", Err: timeoutErr{}}), attempts: 8, waited: backoffTotal, unknown: true},
		{name: "dns failure", attempt: fail(&net.DNSError{Err: "no such host", Name: "api.github.com", IsNotFound: true}), attempts: 8, waited: backoffTotal, unknown: true},
		{name: "tls handshake timeout", attempt: fail(errors.New("net/http: TLS handshake timeout")), attempts: 8, waited: backoffTotal, unknown: true},
		{name: "connection reset", attempt: fail(&net.OpError{Op: "read", Err: syscall.ECONNRESET}), attempts: 8, waited: backoffTotal, unknown: true},
		{name: "unexpected eof", attempt: fail(io.ErrUnexpectedEOF), attempts: 8, waited: backoffTotal, unknown: true},
		{name: "response header timeout", attempt: fail(context.DeadlineExceeded), attempts: 8, waited: backoffTotal, unknown: true},
		{name: "untrusted certificate", attempt: fail(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), attempts: 1},
		{name: "certificate for another host", attempt: fail(x509.HostnameError{Host: "api.github.com"}), attempts: 1},
		{name: "503", attempt: status(503, nil), attempts: 8, waited: backoffTotal, code: 503},
		{name: "503 retry-after", attempt: status(503, map[string]string{"Retry-After": "20"}), attempts: 7, waited: 120 * time.Second, code: 503},
		{name: "graphql rate limited", url: "https://api.github.com/graphql", attempt: graphQLRateLimitedResp(map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(testNow.Add(19 * time.Second).Unix())}), attempts: 8, waited: 26 * time.Second, code: 200},
		{name: "graphql error that is an answer", url: "https://api.github.com/graphql", attempt: graphQLBody(`{"errors":[{"type":"NOT_FOUND","path":["a0"]}]}`), attempts: 1, code: 200},
		{name: "500", attempt: status(500, nil), attempts: 8, waited: backoffTotal, code: 500},
		{name: "429 retry-after seconds", attempt: status(429, map[string]string{"Retry-After": "5"}), attempts: 8, waited: 35 * time.Second, code: 429},
		{name: "429 without headers backs off", attempt: status(429, nil), attempts: 8, waited: backoffTotal, code: 429},
		{name: "secondary limit retry-after http date", attempt: status(403, map[string]string{"Retry-After": testNow.Add(7 * time.Second).Format(http.TimeFormat)}), attempts: 8, waited: 13 * time.Second, code: 403},
		{name: "retry-after of minutes is waited out", attempt: status(429, map[string]string{"Retry-After": "60"}), attempts: 3, waited: 120 * time.Second, code: 429},
		{name: "retry-after past the outage window fails fast", attempt: status(429, map[string]string{"Retry-After": "600"}), attempts: 1, code: 429},
		{name: "primary limit reset soon", attempt: status(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(testNow.Add(9 * time.Second).Unix())}), attempts: 8, waited: 16 * time.Second, code: 403},
		{name: "primary limit reset past the window fails fast", attempt: status(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(testNow.Add(3 * time.Minute).Unix())}), attempts: 1, code: 403},
		{name: "primary limit reset in 40m fails fast", attempt: status(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(testNow.Add(40 * time.Minute).Unix())}), attempts: 1, code: 403},
		{name: "plain 403 is an answer", attempt: status(403, nil), attempts: 1, code: 403},
		{name: "401 is an answer", attempt: status(401, nil), attempts: 1, code: 401},
		{name: "404 is an answer", attempt: status(404, nil), attempts: 1, code: 404},
		{name: "409 is an answer", attempt: status(409, nil), attempts: 1, code: 409},
		{name: "422 is an answer", attempt: status(422, nil), attempts: 1, code: 422},
		{name: "200", attempt: status(200, nil), attempts: 1, code: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget, slept := fakeBudget()
			calls := 0
			rt := newRetryTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return tt.attempt()
			}), budget)
			url := tt.url
			if url == "" {
				url = "https://api.github.com/repos/o/r"
			}
			req, _ := http.NewRequest(http.MethodGet, url, nil)
			resp, err := rt.RoundTrip(req)

			assert.Equal(t, tt.attempts, calls, "attempts")
			assert.Equal(t, tt.waited, *slept, "total wait")
			if tt.code != 0 {
				require.NoError(t, err)
				assert.Equal(t, tt.code, resp.StatusCode)
				resp.Body.Close()
				return
			}
			require.Error(t, err)
			var unknown *UnknownOutcomeError
			assert.Equal(t, tt.unknown, errors.As(err, &unknown), "unknown outcome: %v", err)
		})
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

func TestRetryTransport_RecoversAfterOutage(t *testing.T) {
	budget, slept := fakeBudget()
	calls := 0
	rt := newRetryTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return status(502, nil)()
		}
		return status(200, nil)()
	}), budget)
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, time.Second, *slept)
}

// TestRetryTransport_OutageWindowIsShared: requests waiting out one outage
// share its window, so a run gives up about two minutes after GitHub stops
// answering, however many requests it makes; an answer reopens it.
func TestRetryTransport_OutageWindowIsShared(t *testing.T) {
	budget, slept := fakeBudget()
	up := false
	n := 0
	rt := newRetryTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		n++
		if up {
			return status(200, nil)()
		}
		return status(429, map[string]string{"Retry-After": "40"})()
	}), budget)
	calls := func() int {
		n = 0
		req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)
		resp.Body.Close()
		return n
	}
	assert.Equal(t, 4, calls(), "waits 40s three times, the whole window")
	assert.Equal(t, 1, calls(), "the window is spent")
	assert.Equal(t, 120*time.Second, *slept)
	up = true
	assert.Equal(t, 1, calls())
	up = false
	assert.Equal(t, 4, calls(), "an answer reopens the window")
}

// TestRetryTransport_Notices: each wait says why and for how long, so a
// long wait doesn't look like a hang.
func TestRetryTransport_Notices(t *testing.T) {
	tests := []struct {
		name    string
		attempt func() (*http.Response, error)
		want    string
	}{
		{"server error", status(503, nil), "GitHub API unavailable (503 Service Unavailable); retrying in 1s, attempt 2/8"},
		{"refused", fail(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), "GitHub API unreachable (connection refused); retrying in 1s, attempt 2/8"},
		{"dns", fail(&net.DNSError{Err: "no such host", Name: "api.github.com"}), "GitHub API unreachable (DNS lookup failed); retrying in 1s, attempt 2/8"},
		{"rate limit", status(429, map[string]string{"Retry-After": "90"}), "GitHub API rate limit; waiting until 14:01 UTC, attempt 2/8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget, _ := fakeBudget()
			var got []string
			budget.notify = func(msg string) { got = append(got, msg) }
			rt := newRetryTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { return tt.attempt() }), budget)
			req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
			resp, err := rt.RoundTrip(req)
			if err == nil {
				resp.Body.Close()
			}
			require.NotEmpty(t, got)
			assert.Equal(t, tt.want, got[0])
		})
	}
}

// TestRetryTransport_NoticesAreThrottled: concurrent requests waiting out
// the same outage print one line, not one each.
func TestRetryTransport_NoticesAreThrottled(t *testing.T) {
	budget, _ := fakeBudget()
	budget.sleep = func(context.Context, time.Duration) {}
	var got []string
	budget.notify = func(msg string) { got = append(got, msg) }
	rt := newRetryTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { return status(502, nil)() }), budget)
	for range 5 {
		req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)
		resp.Body.Close()
	}
	assert.Len(t, got, 1)
}

func TestRetryTransport_CallerCancelStops(t *testing.T) {
	budget, _ := fakeBudget()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	rt := newRetryTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, context.Canceled
	}), budget)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/x", nil)
	_, err := rt.RoundTrip(req)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
	assert.False(t, errors.As(err, new(*UnknownOutcomeError)))
}

// TestRetryTransport_BodyOutlivesRoundTrip: the per-attempt timeout must
// stay live until the caller closes the body it is reading.
func TestRetryTransport_BodyOutlivesRoundTrip(t *testing.T) {
	budget, _ := fakeBudget()
	rt := newRetryTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: ctxBody{r.Context()}}, nil
	}), budget)
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(b))
	require.NoError(t, resp.Body.Close())
}

type ctxBody struct{ ctx context.Context }

func (b ctxBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return copy(p, "ok"), io.EOF
}

func (ctxBody) Close() error { return nil }

func TestRetryTransport_ResendsBodyOnRetry(t *testing.T) {
	const want = `{"query":"some graphql"}`
	budget, _ := fakeBudget()
	calls := 0
	rt := newRetryTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		got, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, want, string(got), "attempt %d", calls)
		if calls < 3 {
			return status(500, nil)()
		}
		return status(200, nil)()
	}), budget)
	req, _ := http.NewRequest(http.MethodPost, "https://api.github.com/graphql", bytes.NewReader([]byte(want)))
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, 3, calls)
}

func TestRetryAt(t *testing.T) {
	reset := time.Unix(testNow.Add(38*time.Minute).Unix(), 0)
	tests := []struct {
		name string
		err  error
		want time.Time
	}{
		{"primary limit", httpErr(403, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}), reset.Add(time.Second)},
		{"http date", httpErr(429, http.Header{"Retry-After": {reset.UTC().Format(http.TimeFormat)}}), reset},
		{"unknown outcome", &UnknownOutcomeError{Cause: io.EOF, RetryAt: reset}, reset},
		{"no hint", httpErr(503, nil), time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, tt.want.Equal(RetryAt(tt.err)), "got %v", RetryAt(tt.err))
		})
	}
}

// TestRetryTransport_QuickPolicy: outside CI a run retries a blip for a few
// seconds, then reports, and doesn't wait out a rate limit.
func TestRetryTransport_QuickPolicy(t *testing.T) {
	tests := []struct {
		name     string
		attempt  func() (*http.Response, error)
		attempts int
		waited   time.Duration
	}{
		{"503", status(503, nil), 3, 3 * time.Second},
		{"refused", fail(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), 3, 3 * time.Second},
		{"retry-after a few seconds", status(429, map[string]string{"Retry-After": "3"}), 3, 6 * time.Second},
		{"retry-after a minute", status(429, map[string]string{"Retry-After": "60"}), 1, 0},
		{"404", status(404, nil), 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget, slept := fakeBudget()
			budget.policy = quickPolicy
			calls := 0
			rt := newRetryTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return tt.attempt()
			}), budget)
			req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
			if resp, err := rt.RoundTrip(req); err == nil {
				resp.Body.Close()
			}
			assert.Equal(t, tt.attempts, calls)
			assert.Equal(t, tt.waited, *slept)
		})
	}
}

func TestRunPolicy(t *testing.T) {
	for _, tt := range []struct {
		ci, actions, dependabot string
		want                    retryPolicy
	}{
		{"true", "", "", patientPolicy},
		{"", "true", "", patientPolicy},
		{"", "", "1", patientPolicy},
		{"", "", "", quickPolicy},
		{"false", "", "", quickPolicy},
		{"", "", "0", quickPolicy},
	} {
		t.Setenv("CI", tt.ci)
		t.Setenv("GITHUB_ACTIONS", tt.actions)
		t.Setenv("GH_ACTIONS_LOCK_DEPENDABOT_PROXY", tt.dependabot)
		assert.Equal(t, tt.want, runPolicy(), "CI=%q GITHUB_ACTIONS=%q GH_ACTIONS_LOCK_DEPENDABOT_PROXY=%q", tt.ci, tt.actions, tt.dependabot)
	}
}
