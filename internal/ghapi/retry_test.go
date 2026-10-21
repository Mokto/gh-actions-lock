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
	var slept time.Duration
	b.now = func() time.Time { return testNow.Add(slept) }
	b.sleep = func(_ context.Context, d time.Duration) { slept += d }
	b.jitter = func(max time.Duration) time.Duration { return max }
	return b, &slept
}

func status(code int, h map[string]string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		header := http.Header{}
		for k, v := range h {
			header.Set(k, v)
		}
		return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(bytes.NewReader(nil))}, nil
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
		attempt  func() (*http.Response, error)
		attempts int
		waited   time.Duration
		// unknown is true when the caller gets an UnknownOutcomeError,
		// false when it gets an error that is an answer; zero code means
		// an error is expected.
		unknown bool
		code    int
	}{
		{name: "dial refused", attempt: fail(refused), attempts: 3, waited: 3 * time.Second, unknown: true},
		{name: "dial timeout", attempt: fail(&net.OpError{Op: "dial", Err: timeoutErr{}}), attempts: 3, waited: 3 * time.Second, unknown: true},
		{name: "dns failure", attempt: fail(&net.DNSError{Err: "no such host", Name: "api.github.com", IsNotFound: true}), attempts: 3, waited: 3 * time.Second, unknown: true},
		{name: "tls handshake timeout", attempt: fail(errors.New("net/http: TLS handshake timeout")), attempts: 3, waited: 3 * time.Second, unknown: true},
		{name: "connection reset", attempt: fail(&net.OpError{Op: "read", Err: syscall.ECONNRESET}), attempts: 3, waited: 3 * time.Second, unknown: true},
		{name: "unexpected eof", attempt: fail(io.ErrUnexpectedEOF), attempts: 3, waited: 3 * time.Second, unknown: true},
		{name: "response header timeout", attempt: fail(context.DeadlineExceeded), attempts: 3, waited: 3 * time.Second, unknown: true},
		{name: "untrusted certificate", attempt: fail(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), attempts: 1},
		{name: "certificate for another host", attempt: fail(x509.HostnameError{Host: "api.github.com"}), attempts: 1},
		{name: "503", attempt: status(503, nil), attempts: 3, waited: 3 * time.Second, code: 503},
		{name: "500", attempt: status(500, nil), attempts: 3, waited: 3 * time.Second, code: 500},
		{name: "429 retry-after seconds", attempt: status(429, map[string]string{"Retry-After": "5"}), attempts: 3, waited: 10 * time.Second, code: 429},
		{name: "429 without headers backs off", attempt: status(429, nil), attempts: 3, waited: 3 * time.Second, code: 429},
		{name: "secondary limit retry-after http date", attempt: status(403, map[string]string{"Retry-After": testNow.Add(7 * time.Second).Format(http.TimeFormat)}), attempts: 3, waited: 7 * time.Second, code: 403},
		{name: "retry-after past the budget fails fast", attempt: status(429, map[string]string{"Retry-After": "120"}), attempts: 1, code: 429},
		{name: "primary limit reset soon", attempt: status(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(testNow.Add(9 * time.Second).Unix())}), attempts: 3, waited: 10 * time.Second, code: 403},
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
			req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/o/r", nil)
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

func TestRetryTransport_RunBudgetIsShared(t *testing.T) {
	budget, slept := fakeBudget()
	rt := newRetryTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return status(429, map[string]string{"Retry-After": "25"})()
	}), budget)
	calls := func() int {
		n := 0
		inner := rt.inner
		rt.inner = roundTripFunc(func(r *http.Request) (*http.Response, error) { n++; return inner.RoundTrip(r) })
		defer func() { rt.inner = inner }()
		req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
		resp, err := rt.RoundTrip(req)
		require.NoError(t, err)
		resp.Body.Close()
		return n
	}
	assert.Equal(t, 3, calls(), "first request waits 25s twice")
	assert.Equal(t, 1, calls(), "10s left can't cover another 25s wait")
	assert.Equal(t, 50*time.Second, *slept)
}

func TestRetryTransport_CircuitBreaker(t *testing.T) {
	budget, _ := fakeBudget()
	calls := map[string]int{}
	rt := newRetryTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls[r.URL.Host]++
		if r.URL.Host == "api.github.com" {
			return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		}
		return status(200, nil)()
	}), budget)
	get := func(host string) error {
		req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/x", nil)
		resp, err := rt.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	for range breakerThreshold {
		require.Error(t, get("api.github.com"))
	}
	before := calls["api.github.com"]
	err := get("api.github.com")
	var unknown *UnknownOutcomeError
	require.ErrorAs(t, err, &unknown)
	assert.Contains(t, err.Error(), "did not respond to the last 3 requests")
	assert.Equal(t, before, calls["api.github.com"], "an open breaker makes no request")
	assert.NoError(t, get("ghe.example.com"), "other hosts are unaffected")
}

func TestRetryTransport_AnswerClosesBreaker(t *testing.T) {
	budget, _ := fakeBudget()
	down := true
	rt := newRetryTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		if down {
			return nil, io.ErrUnexpectedEOF
		}
		return status(404, nil)()
	}), budget)
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)
	for range breakerThreshold - 1 {
		_, err := rt.RoundTrip(req)
		require.Error(t, err)
	}
	down = false
	_, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.False(t, budget.open("api.github.com"))
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
