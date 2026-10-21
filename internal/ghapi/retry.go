package ghapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Retry policy for every GitHub API request. This transport is the only
// retry layer: callers above it never retry or split a batch because of an
// outage, so one run's total wait stays bounded.
const (
	maxAttempts = 3
	backoffBase = time.Second
	backoffCap  = 8 * time.Second
	// runRetryBudget caps the time one run spends waiting between retries,
	// across all requests and hosts.
	runRetryBudget = 60 * time.Second
	// breakerThreshold is how many consecutive requests to one host may
	// exhaust their retries before later requests to it fail at once.
	breakerThreshold = 3

	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 30 * time.Second
	requestTimeout        = 60 * time.Second
)

// UnknownOutcomeError reports a request that ended without an answer from
// GitHub: the connection failed, timed out, or was never attempted because
// the host stopped responding. Retrying later may succeed. RetryAt, when
// set, is the earliest time GitHub said a retry could succeed.
type UnknownOutcomeError struct {
	Cause   error
	RetryAt time.Time
}

func (e *UnknownOutcomeError) Error() string { return e.Cause.Error() }

func (e *UnknownOutcomeError) Unwrap() error { return e.Cause }

// retryBudget is shared by every transport of one Client so a run's retries
// draw on one time budget and one circuit breaker per host.
type retryBudget struct {
	mu        sync.Mutex
	remaining time.Duration
	failures  map[string]int

	now    func() time.Time
	sleep  func(context.Context, time.Duration)
	jitter func(max time.Duration) time.Duration
}

func newRetryBudget() *retryBudget {
	return &retryBudget{
		remaining: runRetryBudget,
		failures:  map[string]int{},
		now:       time.Now,
		sleep:     DefaultSleep,
		jitter: func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return rand.N(max + 1)
		},
	}
}

// reserve claims d from the budget, reporting false when it doesn't fit.
func (b *retryBudget) reserve(d time.Duration) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d > b.remaining {
		return false
	}
	b.remaining -= d
	return true
}

func (b *retryBudget) open(host string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures[host] >= breakerThreshold
}

// record notes how a request to host ended: gaveUp when it ran out of
// retries without an answer. Any answer closes the breaker.
func (b *retryBudget) record(host string, gaveUp bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if gaveUp {
		b.failures[host]++
		return
	}
	delete(b.failures, host)
}

// retryTransport retries requests that ended without an answer: transport
// failures, 5xx, and rate limits. Everything it sends is an idempotent read
// (REST GETs and GraphQL queries), so a retry is always safe.
type retryTransport struct {
	inner  http.RoundTripper
	budget *retryBudget
}

func newRetryTransport(inner http.RoundTripper, budget *retryBudget) *retryTransport {
	if inner == nil {
		inner = http.DefaultTransport
	}
	if budget == nil {
		budget = newRetryBudget()
	}
	return &retryTransport{inner: inner, budget: budget}
}

func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if rt.budget.open(host) {
		return nil, &UnknownOutcomeError{Cause: fmt.Errorf("skipped: %s did not respond to the last %d requests", host, breakerThreshold)}
	}
	// Snapshot the body so POST requests can be replayed on retry.
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading request body for retry: %w", err)
		}
	}
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(req.Context(), requestTimeout)
		try := req.Clone(ctx)
		if body != nil {
			try.Body = io.NopCloser(bytes.NewReader(body))
			try.ContentLength = int64(len(body))
		}
		resp, err := rt.inner.RoundTrip(try)
		if err != nil {
			cancel()
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			if !retryableTransportErr(err) {
				return nil, err
			}
			if !rt.wait(req.Context(), attempt, time.Time{}) {
				rt.budget.record(host, true)
				return nil, &UnknownOutcomeError{Cause: err}
			}
			continue
		}
		if retryAt, ok := retryableResponse(resp, rt.budget.now()); ok {
			if rt.wait(req.Context(), attempt, retryAt) {
				drain(resp.Body)
				cancel()
				continue
			}
			rt.budget.record(host, true)
		} else {
			rt.budget.record(host, false)
		}
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
}

// wait sleeps before the next attempt and reports whether one should be
// made. A wait GitHub asked for (retryAt) is honored only if it fits the
// run's remaining budget; otherwise the caller gets the answer now rather
// than a run that stalls until the reset.
func (rt *retryTransport) wait(ctx context.Context, attempt int, retryAt time.Time) bool {
	if attempt >= maxAttempts || ctx.Err() != nil {
		return false
	}
	var d time.Duration
	if retryAt.IsZero() {
		d = rt.budget.jitter(min(backoffCap, backoffBase<<(attempt-1)))
	} else {
		d = max(retryAt.Sub(rt.budget.now()), 0)
	}
	if !rt.budget.reserve(d) {
		return false
	}
	rt.budget.sleep(ctx, d)
	return ctx.Err() == nil
}

// retryableResponse reports whether resp is an outage or rate limit worth
// retrying, and when GitHub said a retry could succeed (zero if it didn't).
func retryableResponse(resp *http.Response, now time.Time) (time.Time, bool) {
	switch {
	case resp.StatusCode >= 500:
		return time.Time{}, true
	case resp.StatusCode == http.StatusTooManyRequests, rateLimited403(resp.StatusCode, resp.Header):
		return retryAfter(resp.Header, now), true
	}
	return time.Time{}, false
}

// rateLimited403 reports a 403 that is a rate limit rather than a
// permission answer: a secondary limit (Retry-After) or an exhausted
// primary budget.
func rateLimited403(code int, h http.Header) bool {
	return code == http.StatusForbidden && (h.Get("Retry-After") != "" || h.Get("X-RateLimit-Remaining") == "0")
}

// retryAfter returns when a rate-limited request may be retried, from
// Retry-After (seconds or an HTTP date) or X-RateLimit-Reset.
func retryAfter(h http.Header, now time.Time) time.Time {
	if ra := h.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return now.Add(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(ra); err == nil {
			return t
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if ts, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(ts, 0).Add(time.Second)
		}
	}
	return time.Time{}
}

// RetryAt returns the earliest time GitHub said a retry of the request
// behind err could succeed, or the zero time if it didn't say.
func RetryAt(err error) time.Time {
	var unknown *UnknownOutcomeError
	if errors.As(err, &unknown) && !unknown.RetryAt.IsZero() {
		return unknown.RetryAt
	}
	if h, ok := errHeaders(err); ok {
		return retryAfter(h, time.Now())
	}
	return time.Time{}
}

// retryableTransportErr reports whether a transport failure may clear up on
// retry. A certificate GitHub's TLS chain doesn't satisfy is configuration
// (a proxy or CA bundle), not an outage.
func retryableTransportErr(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.Canceled):
		return false
	case errors.As(err, &unknownAuthority), errors.As(err, &hostname),
		errors.As(err, &invalid), errors.As(err, &verify):
		return false
	}
	return true
}

func drain(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	body.Close()
}

// cancelOnClose releases an attempt's timeout once the caller has read the
// response, not when RoundTrip returns.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// apiTransport is the base transport for GitHub API requests: the default
// transport, with timeouts so a stalled connection fails instead of hanging
// the run.
func apiTransport(insecure bool) http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t = t.Clone()
	t.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = tlsHandshakeTimeout
	t.ResponseHeaderTimeout = responseHeaderTimeout
	if insecure {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // integration tests only
	}
	return t
}
