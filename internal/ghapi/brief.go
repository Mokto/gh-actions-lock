package ghapi

import (
	"fmt"
	"net/http"
)

// Brief formats err for one terminal line: an HTTP failure as its status
// alone, without the request URL. The run log and --json keep the full error.
func Brief(err error) string {
	if code, ok := StatusCode(err); ok {
		return fmt.Sprintf("GitHub returned %d %s", code, http.StatusText(code))
	}
	return err.Error()
}

// BriefError is an error whose message is short enough for the terminal.
// Err carries the full message for --json and the run log.
type BriefError struct {
	Msg string
	Err error
}

func (e *BriefError) Error() string { return e.Msg }

func (e *BriefError) Unwrap() error { return e.Err }

// Verbose returns the full message.
func (e *BriefError) Verbose() string { return e.Err.Error() }
