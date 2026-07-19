package forward

import (
	"math/rand/v2"
	"time"
)

const (
	backoffBase = 1 * time.Second
	backoffCap  = 30 * time.Second
)

// Backoff returns the delay before reconnect attempt n (0-based):
// exponential with full jitter, capped. Full jitter (uniform in
// [0, min(cap, base*2^n)]) avoids thundering-herd reconnects from many
// forwarders after a shared server restart.
func Backoff(attempt int) time.Duration {
	max := backoffCap
	if attempt < 5 { // base<<5 == 32s, already past the cap
		if d := backoffBase << uint(attempt); d < max {
			max = d
		}
	}
	// Uniform in [max/2, max]: keeps some jitter without ever retrying
	// absurdly fast on later attempts.
	half := max / 2
	return half + rand.N(max-half+1)
}
