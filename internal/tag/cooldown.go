// Package tag lists tags and classifies versions.
package tag

import "strings"

// CooldownConfig is the configured minimum release age per action. It only
// sets the fresh-tag warning window; it does not filter what gets pinned.
// DefaultDays applies to every action; RepoOverrides sets a per-repo
// (owner/name) value that wins over the default.
type CooldownConfig struct {
	DefaultDays   int
	RepoOverrides map[string]int
}

// CooldownDays returns the cooldown period for a repo, falling back to the default.
func (c CooldownConfig) CooldownDays(owner, repo string) int {
	if days, ok := c.RepoOverrides[owner+"/"+repo]; ok {
		return days
	}
	// Override keys may be written with different owner/repo casing than the
	// canonicalized lookup; fall back to a case-insensitive match.
	want := strings.ToLower(owner + "/" + repo)
	for k, days := range c.RepoOverrides {
		if strings.ToLower(k) == want {
			return days
		}
	}
	return c.DefaultDays
}
