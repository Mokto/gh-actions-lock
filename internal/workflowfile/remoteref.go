package workflowfile

import (
	"fmt"
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// MalformedRemoteRef reports whether a `uses:` value names a remote action
// the parser can't accept, such as owner/repo@main:action.yml. Forms that
// are not repository actions by design (local paths, docker images,
// expressions, `$/…`, reusable workflows) are not malformed; callers skip
// those, but a malformed remote reference must fail instead of being skipped.
func MalformedRemoteRef(use string) bool {
	use = strings.TrimSpace(use)
	switch {
	case use == "",
		strings.HasPrefix(use, "./"),
		strings.HasPrefix(use, "docker://"),
		strings.Contains(use, "${"),
		IsSelfRepositoryAction(use):
		return false
	}
	return parserlock.ParseActionRef(use) == nil && parserlock.ParseReusableWorkflowRef(use) == nil
}

// MalformedRefError is the error for a malformed remote `uses:` value.
func MalformedRefError(use string) error {
	return fmt.Errorf("uses: %s is not an action reference the Actions backend accepts", strings.TrimSpace(use))
}
