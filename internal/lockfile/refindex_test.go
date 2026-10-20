package lockfile

import (
	"strings"
	"testing"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefIndexLookup(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	upper := strings.ToUpper(sha)
	// One commit recorded under two spellings, with different provenance
	// and edges, plus a branch, a dangling root and a dangling edge.
	file, err := parserlock.Parse([]byte(`version: 'v0.0.3'
workflows:
  '.github/workflows/ci.yml':
    - 'o/a@` + sha + `'
    - 'o/a@` + upper + `'
    - 'o/b@main'
    - 'o/gone@v1'
dependencies:
  'o/a@` + sha + `':
    ref: 'v1.0.0'
    commit: 'sha1-` + sha + `'
    owner_id: 1
    repo_id: 1
    uses:
      - 'o/child@v2'
  'o/a@` + upper + `':
    ref: 'v1'
    commit: 'sha1-` + sha + `'
    owner_id: 1
    repo_id: 1
  'o/b@main':
    ref: 'main'
    commit: 'sha1-` + sha + `'
    owner_id: 1
    repo_id: 2
`))
	require.NoError(t, err)
	x := NewRefIndex(file)

	for _, tt := range []struct {
		name, nwo, ref string
		want           RefMatch
	}{
		{
			name: "exact spelling of an ambiguous class", nwo: "O/A", ref: sha,
			want: RefMatch{Exact: "o/a@" + sha, Candidates: []string{"o/a@" + upper, "o/a@" + sha}},
		},
		{
			name: "third spelling of an ambiguous class has no exact match", nwo: "o/a", ref: "AbCdEf0123456789abcdef0123456789abcdef01",
			want: RefMatch{Candidates: []string{"o/a@" + upper, "o/a@" + sha}},
		},
		{name: "branch casing is another ref", nwo: "o/b", ref: "Main", want: RefMatch{}},
		{name: "branch", nwo: "O/B", ref: "main", want: RefMatch{Exact: "o/b@main", Candidates: []string{"o/b@main"}}},
		{name: "dangling root is spelled but not an entry", nwo: "o/gone", ref: "v1", want: RefMatch{Exact: "o/gone@v1"}},
		{name: "dangling edge is spelled but not an entry", nwo: "o/child", ref: "v2", want: RefMatch{Exact: "o/child@v2"}},
		{name: "unrecorded", nwo: "o/new", ref: "v1", want: RefMatch{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, x.Lookup(tt.nwo, tt.ref))
		})
	}

	t.Run("candidates keep their own metadata", func(t *testing.T) {
		m := x.Lookup("o/a", sha)
		lower, up := file.Dependencies[m.Candidates[1]], file.Dependencies[m.Candidates[0]]
		assert.Equal(t, "v1.0.0", lower.Ref)
		assert.Equal(t, []string{"o/child@v2"}, lower.Uses)
		assert.Equal(t, "v1", up.Ref)
		assert.Empty(t, up.Uses)
	})
}

func TestRefMatchEntries(t *testing.T) {
	for _, tt := range []struct {
		name string
		m    RefMatch
		want []string
	}{
		{name: "exact entry wins among candidates", m: RefMatch{Exact: "o/a@B", Candidates: []string{"o/a@B", "o/a@b"}}, want: []string{"o/a@B"}},
		{name: "single candidate reached from another spelling", m: RefMatch{Candidates: []string{"o/a@b"}}, want: []string{"o/a@b"}},
		{name: "several candidates and no exact entry", m: RefMatch{Candidates: []string{"o/a@B", "o/a@b"}}, want: []string{"o/a@B", "o/a@b"}},
		{name: "dangling exact spelling does not hide several candidates", m: RefMatch{Exact: "o/a@Bb", Candidates: []string{"o/a@B", "o/a@b"}}, want: []string{"o/a@B", "o/a@b"}},
		{name: "dangling exact spelling does not shadow a single candidate", m: RefMatch{Exact: "o/a@Bb", Candidates: []string{"o/a@b"}}, want: []string{"o/a@b"}},
		{name: "dangling exact spelling alone is not an entry", m: RefMatch{Exact: "o/gone@v1"}},
		{name: "unrecorded", m: RefMatch{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.m.Entries())
		})
	}
}
