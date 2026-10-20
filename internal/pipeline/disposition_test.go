package pipeline

import (
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/pipeline/checks"
	"github.com/stretchr/testify/assert"
)

func TestDisposeParity(t *testing.T) {
	const sha = "373d742f768e37e4e68d84ada06be93fb2ce64d8"
	shaPin := parserlock.Pin{Owner: "actions", Repo: "checkout", Ref: sha}
	tagPin := parserlock.Pin{Owner: "actions", Repo: "checkout", Ref: "v4"}
	relock := parityMode{relock: true}
	acceptMoved := parityMode{relock: true, acceptMoved: true}
	tests := []struct {
		name     string
		mode     parityMode
		category checks.Category
		parent   string
		subject  paritySubject
		wantKeep bool
		wantSev  checks.Severity
	}{
		{name: "accept-moved repairs a direct full-SHA pin", mode: acceptMoved, subject: paritySubject{pin: shaPin, replaced: true}, wantKeep: true, wantSev: checks.SeverityWarning},
		{name: "accept-moved keeps a transitive full-SHA pin blocking", mode: acceptMoved, parent: "octo/composite@v1", subject: paritySubject{pin: shaPin, replaced: true}, wantKeep: true, wantSev: checks.SeverityError},
		{name: "accept-moved can't repair a pin that didn't resolve", mode: acceptMoved, subject: paritySubject{pin: shaPin}, wantKeep: true, wantSev: checks.SeverityError},
		{name: "relock keeps a full-SHA pin blocking", mode: relock, subject: paritySubject{pin: shaPin, replaced: true}, wantKeep: true, wantSev: checks.SeverityError},
		{name: "relock drops a mutable pin it re-resolves", mode: relock, subject: paritySubject{pin: tagPin, replaced: true}},
		{name: "relock keeps a mutable pin it couldn't re-resolve", mode: relock, subject: paritySubject{pin: tagPin}, wantKeep: true, wantSev: checks.SeverityError},
		{name: "relock drops an inconclusive mutable pin it re-resolves", category: checks.ReachabilityUnknown, mode: relock, subject: paritySubject{pin: tagPin, head: "v4", inconclusive: true, replaced: true}},
		{name: "relock blocks an inconclusive mutable pin it couldn't re-resolve", category: checks.ReachabilityUnknown, mode: relock, subject: paritySubject{pin: tagPin, head: "v4", inconclusive: true}, wantKeep: true, wantSev: checks.SeverityError},
		{name: "plain run keeps everything", subject: paritySubject{pin: shaPin}, wantKeep: true, wantSev: checks.SeverityError},
		{name: "relock keeps a fresh pin it would write", mode: relock, subject: paritySubject{pin: tagPin, fresh: true}, wantKeep: true, wantSev: checks.SeverityError},
		{name: "an inconclusive fresh pin blocks", category: checks.ReachabilityUnknown, mode: relock, subject: paritySubject{pin: tagPin, fresh: true, inconclusive: true}, wantKeep: true, wantSev: checks.SeverityError},
		{name: "an inconclusive history check blocks", category: checks.ReachabilityUnknown, subject: paritySubject{pin: tagPin, head: "v4", inconclusive: true}, wantKeep: true, wantSev: checks.SeverityError},
		{name: "an inconclusive check without a ref warns", category: checks.ReachabilityUnknown, subject: paritySubject{pin: tagPin, inconclusive: true}, wantKeep: true, wantSev: checks.SeverityWarning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := checks.Finding{Category: checks.UnreachablePin, Severity: checks.SeverityError, ParentNWO: tt.parent}
			if tt.category != "" {
				f.Category, f.Severity = tt.category, checks.SeverityWarning
			}
			assert.Equal(t, tt.wantKeep, disposeParity(tt.mode, &f, tt.subject))
			if tt.wantKeep {
				assert.Equal(t, tt.wantSev, f.Severity)
			}
		})
	}
}
