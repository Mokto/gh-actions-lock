package ghapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"testing"

	"github.com/github/gh-actions-lock/internal/ghapi/httpmock"
	"github.com/github/gh-actions-lock/internal/testref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvePinRef(t *testing.T) {
	const (
		tagSHA    = "1111111111111111111111111111111111111111"
		branchSHA = "2222222222222222222222222222222222222222"
	)
	sha := func(s string) httpmock.Responder { return httpmock.JSONResponse(map[string]any{"sha": s}) }
	missing := httpmock.StatusResponse(http.StatusNotFound)
	tests := []struct {
		name      string
		ref       string
		tag       httpmock.Responder
		branch    httpmock.Responder
		commit    httpmock.Responder
		want      string
		ambiguous bool
		notFound  bool
	}{
		{name: "tag", ref: "v1", tag: sha(tagSHA), branch: missing, want: tagSHA},
		{name: "branch", ref: "release/1", tag: missing, branch: sha(branchSHA), want: branchSHA},
		{name: "tag and branch agree", ref: "v1", tag: sha(tagSHA), branch: sha(tagSHA), want: tagSHA},
		{name: "tag and branch disagree", ref: "v1", tag: sha(tagSHA), branch: sha(branchSHA), ambiguous: true},
		{name: "commit", ref: tagSHA, commit: sha(tagSHA), want: tagSHA},
		{name: "missing commit", ref: tagSHA, commit: missing, notFound: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			if tt.commit != nil {
				reg.Register(httpmock.REST("GET", "^/repos/o/r/commits/"+regexp.QuoteMeta(tt.ref)+"$"), tt.commit)
			} else {
				reg.Register(httpmock.REST("GET", "^/repos/o/r/commits/refs/tags/"+regexp.QuoteMeta(tt.ref)+"$"), tt.tag)
				reg.Register(httpmock.REST("GET", "^/repos/o/r/commits/refs/heads/"+regexp.QuoteMeta(tt.ref)+"$"), tt.branch)
			}

			got, err := newTestClient(t, reg).ResolvePinRef(context.Background(), "o", "r", testref.Pin(tt.ref))

			var ambiguous *AmbiguousRefError
			var notFound *RefNotFoundError
			switch {
			case tt.ambiguous:
				require.ErrorAs(t, err, &ambiguous)
			case tt.notFound:
				require.ErrorAs(t, err, &notFound)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
			for _, req := range reg.Requests {
				if tt.commit != nil {
					break
				}
				assert.NotEqual(t, "/repos/o/r/commits/"+tt.ref, req.URL.Path, "a name must never reach rev-parse unqualified")
			}
		})
	}
}

func TestResolveBranch(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(httpmock.REST("GET", "^/repos/o/r/commits/refs/heads/main$"), httpmock.StatusResponse(http.StatusNotFound))

	_, err := newTestClient(t, reg).ResolveBranch(context.Background(), "o", "r", testref.Name("main"))

	var notFound *RefNotFoundError
	require.True(t, errors.As(err, &notFound), "err = %v", err)
}
