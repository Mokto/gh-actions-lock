package ghapi

import (
	"regexp"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// describeSuffix matches the "-g<hex>" ending of a `git describe` name.
var describeSuffix = regexp.MustCompile(`-g[0-9a-fA-F]{4,}$`)

// AcceptedRefName reports whether name is a ref name the Actions backend
// accepts: a valid tag or branch name (see parserlock.ParseRefName) that
// isn't shaped like `git describe` output, which a commit-ish lookup would
// read as an abbreviated object ID.
func AcceptedRefName(name string) bool {
	_, err := parserlock.ParseRefName(name)
	return err == nil && !describeSuffix.MatchString(name)
}
