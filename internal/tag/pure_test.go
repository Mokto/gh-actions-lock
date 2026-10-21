package tag

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRepoInfoIsInternal(t *testing.T) {
	assert.True(t, RepoInfo{Visibility: "private"}.IsInternal())
	assert.True(t, RepoInfo{Visibility: "internal"}.IsInternal())
	assert.False(t, RepoInfo{Visibility: "public"}.IsInternal())
	assert.False(t, RepoInfo{}.IsInternal())
}

func TestURL(t *testing.T) {
	assert.Equal(t,
		"https://github.com/actions/checkout/releases/tag/v4.2.1",
		URL("actions", "checkout", "v4.2.1"))
}

func TestFormatTagAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		iso  string
		want string
	}{
		{"empty", "", ""},
		{"unparseable", "not-a-date", ""},
		{"minutes", now.Add(-30 * time.Minute).Format(time.RFC3339), "30m ago"},
		{"hours", now.Add(-5 * time.Hour).Format(time.RFC3339), "5h ago"},
		{"days", now.Add(-3 * 24 * time.Hour).Format(time.RFC3339), "3d ago"},
		{"months", now.Add(-60 * 24 * time.Hour).Format(time.RFC3339), "2mo ago"},
		{"years", now.Add(-400 * 24 * time.Hour).Format(time.RFC3339), "1y ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, FormatTagAge(tt.iso))
		})
	}
}

func TestReorderSuggestions(t *testing.T) {
	major := Suggestion{Tag: Info{Name: "v4", IsMajor: true}}
	immutable := Suggestion{Tag: Info{Name: "v4.2.1", IsRelease: true, IsImmutable: true}}
	release := Suggestion{Tag: Info{Name: "v4.2.0", IsRelease: true}}
	plain := Suggestion{Tag: Info{Name: "v4.1.9"}}

	// Deliberately scrambled: major first, immutable last.
	got := ReorderSuggestions([]Suggestion{major, plain, release, immutable})

	var names []string
	for _, s := range got {
		names = append(names, s.Tag.Name)
	}
	// Immutable, then regular release, then plain full version, then major-only.
	assert.Equal(t, []string{"v4.2.1", "v4.2.0", "v4.1.9", "v4"}, names)
}
