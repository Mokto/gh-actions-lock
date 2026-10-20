package ghapi

import (
	"strings"
	"unicode/utf8"
)

// ValidRefName reports whether name is a short tag or branch name that
// `git check-ref-format --allow-onelevel` accepts, minus a leading "-".
// A name that fails it may be a revision expression (main~1, v1^, v1@{1})
// that a commit-ish lookup would evaluate instead of resolving literally.
// The rules match actions-lockfile's RefName so the CLI and library agree.
func ValidRefName(name string) bool {
	if name == "" || !utf8.ValidString(name) {
		return false
	}
	if name == "@" || name[0] == '-' || name[0] == '/' || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
		switch c {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return false
		}
	}
	for _, comp := range strings.Split(name, "/") {
		if strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return false
		}
	}
	return true
}
