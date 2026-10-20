package dep

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRefKey(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	for _, tt := range []struct {
		name      string
		a, b      string
		wantEqual bool
	}{
		{name: "owner/repo casing", a: "Actions/Checkout@v4", b: "actions/checkout@v4", wantEqual: true},
		{name: "full SHA casing", a: "o/a@" + strings.ToUpper(sha), b: "o/a@" + sha, wantEqual: true},
		{name: "branch casing is another ref", a: "o/a@Main", b: "o/a@main"},
		{name: "short hex is a name, not a SHA", a: "o/a@ABCDEF0", b: "o/a@abcdef0"},
		{name: "ref containing @ keeps its case", a: "O/A@Release@2024", b: "o/a@release@2024"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantEqual, RefKeyOf(tt.a) == RefKeyOf(tt.b))
			nwo, ref, _ := strings.Cut(tt.a, "@")
			assert.Equal(t, RefKeyOf(tt.a), RefKey(nwo, ref))
		})
	}
}
