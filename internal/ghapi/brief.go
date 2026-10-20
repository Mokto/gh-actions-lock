package ghapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Brief omits HTTP request URLs and keeps authorization guidance, shortening
// other HTTP failures to their status. The run log and --json keep the full error.
func Brief(err error) string {
	var ssoErr *SSORateLimitedError
	if errors.As(err, &ssoErr) {
		return ssoErr.Error()
	}
	if code, ok := StatusCode(err); ok {
		msg := fmt.Sprintf("GitHub returned %d %s", code, http.StatusText(code))
		var httpErr *api.HTTPError
		if IsSAMLEnforcement(err) && errors.As(err, &httpErr) {
			msg += ": " + httpErr.Message
		}
		return msg
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
