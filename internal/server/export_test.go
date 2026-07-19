package server

import "time"

// SetRemoteBackfillTimeoutForTest shortens the relay wait so tests of the
// unanswered-client fallback do not sit out the production timeout. It
// returns a restore function for t.Cleanup.
func SetRemoteBackfillTimeoutForTest(d time.Duration) (restore func()) {
	prev := remoteBackfillTimeout
	remoteBackfillTimeout = d
	return func() { remoteBackfillTimeout = prev }
}
