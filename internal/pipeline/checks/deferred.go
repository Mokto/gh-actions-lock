package checks

import "strings"

// Deferred is a finding on a recorded lock entry that parity excused or
// accepted because the run should replace or remove the entry. It holds
// only if the write the run plans does that; otherwise Finding, as it would
// block with the entry left as recorded, stands.
type Deferred struct {
	// Key is the entry's on-disk key (canonical NWO@ref).
	Key string
	// Ref and Commit are what the entry records.
	Ref    string
	Commit string
	// Finding is the finding as it blocks when the entry stays as recorded.
	Finding Finding
}

// VerifiedKey identifies a commit this run checked for an on-disk key.
func VerifiedKey(key, sha string) string {
	return key + ":" + strings.ToLower(sha)
}
