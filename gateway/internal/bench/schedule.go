package bench

import (
	"math/rand/v2"
	"time"
)

// PoissonArrivals returns the arrival offsets of an open-loop load of rps
// requests per second over d: exponentially distributed gaps, so request
// starts are independent of how long earlier requests take (a closed loop,
// where each worker waits for its response before sending the next, hides
// queueing by slowing down exactly when the target slows down). rng makes a
// schedule reproducible from a seed. A non-positive rate or duration yields
// no arrivals.
func PoissonArrivals(rng *rand.Rand, rps float64, d time.Duration) []time.Duration {
	if rps <= 0 || d <= 0 {
		return nil
	}
	var out []time.Duration
	t := 0.0
	limit := d.Seconds()
	for {
		t += rng.ExpFloat64() / rps
		if t >= limit {
			return out
		}
		out = append(out, time.Duration(t*float64(time.Second)))
	}
}
