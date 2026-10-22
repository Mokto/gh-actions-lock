package ui

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// syncBuffer guards a buffer the spinner goroutines write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestPauseBeforeSpinnerAppears(t *testing.T) {
	tests := []struct {
		name  string
		after time.Duration
		shown bool
	}{
		{"stopping within the grace period never shows it", 0, false},
		{"resuming restarts the grace period", progressGrace + 300*time.Millisecond, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out syncBuffer
			u := &UI{w: &out, noColor: true, isTTY: true}

			u.StartProgress("Checking 3 workflows")
			u.PauseProgress()
			time.Sleep(progressGrace + 100*time.Millisecond)
			assert.NotContains(t, out.String(), "Checking", "a paused pending spinner stays hidden")
			u.ResumeProgress()
			assert.NotContains(t, out.String(), "Checking", "resuming does not show it at once")
			time.Sleep(tt.after)
			u.StopProgress()

			if tt.shown {
				assert.Contains(t, out.String(), "Checking")
			} else {
				assert.NotContains(t, out.String(), "Checking")
			}
		})
	}
}
