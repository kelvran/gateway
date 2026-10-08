package bench

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func TestPoissonArrivalsHaveTheRequestedMeanRate(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	arrivals := PoissonArrivals(rng, 200, 10*time.Second)
	// 200 rps for 10 s: expect ~2000 arrivals; a Poisson count's standard
	// deviation is sqrt(2000) ~ 45, so 5 sigma is well inside [1775, 2225].
	if n := len(arrivals); n < 1775 || n > 2225 {
		t.Fatalf("arrivals = %d, want about 2000 (200 rps for 10 s)", n)
	}
	for i := 1; i < len(arrivals); i++ {
		if arrivals[i] < arrivals[i-1] {
			t.Fatalf("arrival %d (%s) precedes arrival %d (%s)", i, arrivals[i], i-1, arrivals[i-1])
		}
	}
	if last := arrivals[len(arrivals)-1]; last >= 10*time.Second {
		t.Errorf("last arrival at %s, want inside the 10 s window", last)
	}
	// Exponential gaps: coefficient of variation ~ 1 (a fixed-interval
	// scheduler would have ~0).
	var sum, sumSq float64
	for i := 1; i < len(arrivals); i++ {
		g := arrivals[i].Seconds() - arrivals[i-1].Seconds()
		sum += g
		sumSq += g * g
	}
	n := float64(len(arrivals) - 1)
	mean := sum / n
	cv := math.Sqrt(sumSq/n-mean*mean) / mean
	if cv < 0.8 || cv > 1.2 {
		t.Errorf("gap coefficient of variation = %.2f, want ~1 for Poisson arrivals", cv)
	}
}

func TestPoissonArrivalsAreDeterministicForASeed(t *testing.T) {
	a := PoissonArrivals(rand.New(rand.NewPCG(7, 7)), 50, time.Second)
	b := PoissonArrivals(rand.New(rand.NewPCG(7, 7)), 50, time.Second)
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("arrival %d differs: %s vs %s", i, a[i], b[i])
		}
	}
}

func TestPoissonArrivalsRejectNonPositiveRate(t *testing.T) {
	if got := PoissonArrivals(rand.New(rand.NewPCG(1, 1)), 0, time.Second); got != nil {
		t.Errorf("zero rate produced %d arrivals, want none", len(got))
	}
}
