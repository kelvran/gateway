// kelvran-bench is the gateway's benchmark harness: `kelvran-bench upstream`
// serves the OpenAI-shaped mock provider a benchmark points the gateway at,
// and `kelvran-bench run` drives open-loop Poisson load through the gateway
// and writes the measurements (docs/operations/BENCHMARKS.md).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kelvran/gateway/gateway/internal/bench"
	"github.com/kelvran/gateway/gateway/internal/benchupstream"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "upstream":
		err = upstreamCmd(os.Args[2:])
	case "scenarios":
		for _, s := range bench.Scenarios() {
			fmt.Printf("%-4s %s\n", s.Name, s.Description)
		}
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kelvran-bench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kelvran-bench run|upstream|scenarios [flags]")
}

type metaFlag map[string]string

func (m metaFlag) String() string { return fmt.Sprint(map[string]string(m)) }
func (m metaFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("meta %q is not key=value", v)
	}
	m[k] = val
	return nil
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	scenario := fs.String("scenario", "S1a", "preset name (see `kelvran-bench scenarios`)")
	name := fs.String("name", "", "label recorded as the result's scenario (default: the preset name; e.g. S2-baseline for a run straight at the mock)")
	targets := fs.String("targets", "http://127.0.0.1:8080", "comma-separated gateway base URLs (round-robined)")
	keyFile := fs.String("key-file", "", "file holding the virtual-key secret (read, never passed on the command line)")
	model := fs.String("model", "bench", "model name to request")
	rps := fs.Float64("rps", 0, "offered requests per second (0 = the preset's default)")
	duration := fs.Duration("duration", 0, "measured window (0 = the preset's default)")
	warmup := fs.Duration("warmup", -1, "warm-up window, unmeasured (negative = the preset's default)")
	seed := fs.Uint64("seed", 1, "schedule seed")
	gatewayPID := fs.Int("gateway-pid", 0, "gateway pid to sample RSS from (optional)")
	out := fs.String("out", "", "results JSON path (appended to if it exists)")
	benchJSON := fs.String("bench-json", "", "github-action-benchmark customSmallerIsBetter JSON path (appended to)")
	summary := fs.String("summary", "", "markdown summary path (appended to; '-' = stdout)")
	meta := metaFlag{}
	fs.Var(meta, "meta", "key=value recorded in the result (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	preset, ok := bench.LookupScenario(*scenario)
	if !ok {
		return fmt.Errorf("unknown scenario %q", *scenario)
	}
	if *keyFile == "" {
		return errors.New("-key-file is required")
	}
	secret, err := os.ReadFile(*keyFile)
	if err != nil {
		return fmt.Errorf("reading key file: %w", err)
	}
	bearer := strings.TrimSpace(string(secret))
	if bearer == "" {
		return errors.New("key file is empty")
	}
	meta["harness_go"] = runtime.Version()
	meta["harness_gomaxprocs"] = fmt.Sprint(runtime.GOMAXPROCS(0))
	meta["harness_os_arch"] = runtime.GOOS + "/" + runtime.GOARCH

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	steps := preset.RPSSteps
	switch {
	case len(steps) == 0 && *rps > 0:
		steps = []float64{*rps}
	case len(steps) == 0:
		steps = []float64{preset.RPS}
	case *rps > 0:
		// A sweep is defined by its steps; one rate would make it a different experiment.
		fmt.Fprintf(os.Stderr, "kelvran-bench: %s steps through %v rps; -rps %.0f ignored\n", preset.Name, steps, *rps)
	}
	d := preset.Duration
	if *duration > 0 {
		d = *duration
	}
	w := preset.Warmup
	if *warmup >= 0 {
		w = *warmup
	}
	label := preset.Name
	if *name != "" {
		label = *name
	}
	var results []bench.Result
	var interrupted bool
	for _, step := range steps {
		stepName := label
		if len(preset.RPSSteps) > 0 {
			stepName = fmt.Sprintf("%s-%.0frps", label, step)
		}
		cfg := bench.Config{
			Scenario: stepName, Targets: strings.Split(*targets, ","), Bearer: bearer, Model: *model, Stream: preset.Stream,
			RPS: step, Warmup: w, Duration: d, Seed: *seed, PromptPool: preset.PromptPool, GatewayPID: *gatewayPID, Meta: meta,
		}
		fmt.Fprintf(os.Stderr, "kelvran-bench: %s at %.0f rps for %s (+%s warm-up) against %s\n", stepName, step, d, w, *targets)
		res, err := bench.Run(ctx, cfg)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if err != nil {
			// SIGINT/SIGTERM: keep what was measured, labelled, rather than lose it.
			interrupted = true
			res.Meta = withMeta(res.Meta, "interrupted", "true")
		}
		results = append(results, res)
		fmt.Fprintf(os.Stderr, "kelvran-bench: %s: %d requests, %d errors, p50 %.1f ms, p99 %.1f ms\n", stepName, res.Requests, res.Errors, msOf(res.Latency.P50), msOf(res.Latency.P99))
		if interrupted {
			break
		}
	}
	if *out != "" {
		if err := appendJSON(*out, results); err != nil {
			return err
		}
	}
	switch {
	case *benchJSON != "" && interrupted:
		// A Metric carries no meta, so a partial result would enter the trend
		// indistinguishable from a real regression.
		fmt.Fprintln(os.Stderr, "kelvran-bench: interrupted; not appending to -bench-json")
	case *benchJSON != "":
		if err := appendJSON(*benchJSON, bench.ToBenchmarkMetrics(results)); err != nil {
			return err
		}
	}
	if *summary != "" {
		md := bench.MarkdownSummary(results)
		if *summary == "-" {
			fmt.Print(md)
		} else if err := appendMarkdown(*summary, md); err != nil {
			return err
		}
	}
	if interrupted {
		return errors.New("interrupted; the partial result is written with meta interrupted=true")
	}
	return nil
}

func msOf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// withMeta returns a copy of m with k=v set (m may be shared between steps).
func withMeta(m map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

// appendJSON reads an existing JSON array at path (if any), appends items,
// and writes it back, so several `run` invocations build one file.
func appendJSON[T any](path string, items []T) error {
	var existing []T
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &existing); err != nil {
			return fmt.Errorf("%s holds something other than a JSON array: %w", path, err)
		}
	}
	existing = append(existing, items...)
	b, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// appendMarkdown appends table rows to path, writing the header only when
// the file is new.
func appendMarkdown(path, table string) error {
	lines := strings.SplitN(table, "\n", 3)
	if len(lines) < 3 {
		return nil
	}
	header, rows := lines[0]+"\n"+lines[1]+"\n", lines[2]
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		if _, err := f.WriteString(header); err != nil {
			return err
		}
	}
	_, err = f.WriteString(rows)
	return err
}

func upstreamCmd(args []string) error {
	fs := flag.NewFlagSet("upstream", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9001", "address to serve on")
	scenario := fs.String("scenario", "", "preset whose mock-side knobs to use (explicit flags override)")
	latency := fs.Duration("latency", -1, "response latency / time to first chunk")
	chunks := fs.Int("chunks", 0, "streamed content chunks")
	interval := fs.Duration("chunk-interval", -1, "pause between streamed chunks")
	promptTokens := fs.Int("prompt-tokens", 0, "usage.prompt_tokens to report")
	outputTokens := fs.Int("completion-tokens", 0, "usage.completion_tokens to report")
	readyFile := fs.String("ready-file", "", "file to create once listening (for scripts)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := benchupstream.Config{}
	if *scenario != "" {
		preset, ok := bench.LookupScenario(*scenario)
		if !ok {
			return fmt.Errorf("unknown scenario %q", *scenario)
		}
		cfg = benchupstream.Config{Latency: preset.MockLatency, StreamChunks: preset.MockChunks, ChunkInterval: preset.MockChunkInterval, PromptTokens: preset.MockPromptTokens, CompletionTokens: preset.MockOutputTokens}
	}
	if *latency >= 0 {
		cfg.Latency = *latency
	}
	if *chunks > 0 {
		cfg.StreamChunks = *chunks
	}
	if *interval >= 0 {
		cfg.ChunkInterval = *interval
	}
	if *promptTokens > 0 {
		cfg.PromptTokens = *promptTokens
	}
	if *outputTokens > 0 {
		cfg.CompletionTokens = *outputTokens
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", *listen, err)
	}
	srv := &http.Server{Handler: benchupstream.New(cfg), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	if *readyFile != "" {
		// Written only once the port is bound, so a script may trust it.
		if err := os.WriteFile(*readyFile, []byte(ln.Addr().String()+"\n"), 0o600); err != nil {
			return fmt.Errorf("writing ready file: %w", err)
		}
	}
	fmt.Fprintf(os.Stderr, "kelvran-bench upstream: serving on %s (latency %s, chunks %d @ %s, tokens %d/%d)\n", *listen, cfg.Latency, cfg.StreamChunks, cfg.ChunkInterval, cfg.PromptTokens, cfg.CompletionTokens)
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
