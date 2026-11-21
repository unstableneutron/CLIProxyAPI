package auth

import (
	"context"
	"errors"
)

// isCanceledStreamArtifactError reports whether a stream error observed after
// the client canceled may be a consequence of that cancellation. Context errors
// and status-less transport errors qualify; an error carrying an explicit
// upstream HTTP status is an upstream verdict that may have been queued before
// the client disconnected, so it must still be recorded.
func isCanceledStreamArtifactError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return statusCodeFromError(err) <= 0
}
