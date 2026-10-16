package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The Redis round-trip figures come from the gateway's own Prometheus
// endpoint (telemetry.prometheus): the harness is a stdlib-only leaf, so it
// reads the text exposition itself rather than importing a client.
const (
	redisRoundTripsSeries = "kelvran_redis_round_trips_total"
	redisCommandsSeries   = "kelvran_redis_commands_total"
	metricsScrapeTimeout  = 5 * time.Second
	metricsBodyLimit      = 8 << 20
)

// sumPrometheusCounter adds every sample of the named counter across its
// label sets in a text-format exposition. ok is false when the series is
// absent, which is not 0: an exporter that never saw a data point emits
// nothing, and a caller must know the difference.
func sumPrometheusCounter(body, name string) (sum float64, ok bool) {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		switch {
		case strings.HasPrefix(rest, "{"):
			end := strings.LastIndex(rest, "}")
			if end < 0 {
				continue
			}
			rest = rest[end+1:]
		case strings.HasPrefix(rest, " "):
		default:
			continue // a longer name with this one as its prefix (…_total_created)
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		sum += v
		ok = true
	}
	return sum, ok
}

// scrapeRedisCounters GETs every URL and sums the two Redis counters across
// them (one URL per gateway replica). Any failed scrape is an error: a dash
// in the report must mean "not measured", never "could not read". With
// requireSeries (the end scrape, after every request of the window has
// completed) a URL that exposes no round-trip series is an error too: a
// gateway that counted nothing is not a Redis-mode gateway with the hook
// attached, and 0.0 must never pass for a measurement. The start scrape is
// tolerant: with no warm-up the series legitimately does not exist yet.
func scrapeRedisCounters(ctx context.Context, urls []string, requireSeries bool) (roundTrips, commands float64, err error) {
	client := &http.Client{Timeout: metricsScrapeTimeout}
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return 0, 0, fmt.Errorf("bench: metrics endpoint %s: %w", u, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, 0, fmt.Errorf("bench: scraping metrics endpoint %s: %w", u, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, metricsBodyLimit))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, 0, fmt.Errorf("bench: metrics endpoint %s returned %d", u, resp.StatusCode)
		}
		if readErr != nil {
			return 0, 0, fmt.Errorf("bench: reading metrics endpoint %s: %w", u, readErr)
		}
		rt, ok := sumPrometheusCounter(string(body), redisRoundTripsSeries)
		if requireSeries && !ok {
			return 0, 0, fmt.Errorf("bench: metrics endpoint %s exposes no %s series: the gateway counted no Redis round trips (hook not attached, or not a Redis-mode gateway)", u, redisRoundTripsSeries)
		}
		cmds, _ := sumPrometheusCounter(string(body), redisCommandsSeries)
		roundTrips += rt
		commands += cmds
	}
	return roundTrips, commands, nil
}

var errMetricsNotScraped = errors.New("bench: the measured window never started, so the metrics endpoint was not scraped")

// redisPerRequest turns the window's counter deltas into per-request
// figures over the successful requests.
func redisPerRequest(res *Result, startRT, startCmd, endRT, endCmd float64) {
	okRequests := res.Requests - res.Errors
	if okRequests <= 0 {
		return
	}
	rt := (endRT - startRT) / float64(okRequests)
	cmds := (endCmd - startCmd) / float64(okRequests)
	res.RedisRoundTripsPerRequest = &rt
	res.RedisCommandsPerRequest = &cmds
}
