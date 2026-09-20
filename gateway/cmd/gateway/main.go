// Command gateway is Kelvran's single static binary: it loads the static
// YAML config, wires every internal component (identity, rate limiting,
// cache, provider adapters, cost accounting, OTel tracing, the dataplane
// pipeline), and serves /v1/chat/completions in both buffered and
// streaming (SSE) modes.
//
// Streaming (real SSE for OpenAI/Anthropic/Gemini/openaicompat, real
// binary application/vnd.amazon.eventstream for Bedrock via a separate
// path, per docs/rfcs/2026-09-02-streaming-support.md and
// docs/rfcs/2026-09-04-bedrock-converse-stream.md) is wired for every
// registered provider adapter — a streaming request to a future provider
// added without a streaming implementation returns a typed
// dataplane.ErrStreamingNotSupported (HTTP 400), never a silent fallback
// to buffering. OTel spans, per
// docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md, are real for every
// request (buffered or streaming), including a real outer HTTP-server
// span (otelhttp middleware, per that RFC's own named future addition)
// nested around the existing per-request GenAI span. Guardrails
// (pre-call/post-call PII/secrets/prompt-injection checks, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md) are real; MCP
// remains unbuilt.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	berrors "go.etcd.io/bbolt/errors"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/gemini"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/adapter/openaicompat"
	"github.com/kelvran/gateway/gateway/internal/admin"
	"github.com/kelvran/gateway/gateway/internal/alerting"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/budget/boltstore"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/configpropagation"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/guardrail/bedrockguard"
	idempotencyinprocess "github.com/kelvran/gateway/gateway/internal/idempotency/inprocess"
	"github.com/kelvran/gateway/gateway/internal/identity"
	identityboltstore "github.com/kelvran/gateway/gateway/internal/identity/boltstore"
	"github.com/kelvran/gateway/gateway/internal/prompt"
	promptboltstore "github.com/kelvran/gateway/gateway/internal/prompt/boltstore"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/redislimiter"
	"github.com/kelvran/gateway/gateway/internal/router"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// gracefulShutdownTimeout bounds how long http.Server.Shutdown itself
// waits for in-flight connections to go idle before giving up and
// returning — NOT how long an in-flight request-handler goroutine
// actually gets to run. Shutdown never cancels a still-running handler's
// own context when this deadline expires; it just stops waiting on it.
// See postShutdownDrainGrace below for what actually bounds a
// handler's remaining runtime, and drainInFlight's own doc comment for
// why that distinction matters. A fixed default, not a config field —
// nothing in this codebase's own tests runs anywhere close to it, and a
// config knob nobody has asked for yet would be premature.
//
// **Changed 2026-09-17**: a package-level var, not a const, purely so
// graceful_shutdown_integration_test.go's force-exit test can override
// it to a short value for that one test — every real call site
// (run()'s own shutdownBoth) still just reads this at its real,
// unchanged default outside of that test.
var gracefulShutdownTimeout = 30 * time.Second

// postShutdownDrainGrace bounds how much EXTRA time an in-flight
// client-facing request-handler goroutine gets, after
// gracefulShutdownTimeout's own wait gives up, to finish naturally —
// including running its own deferred dataplane.Pipeline.finalize call —
// before main() force-exits via os.Exit. Without this, a request whose
// handler goroutine is still running when Shutdown's deadline expires
// gets killed by os.Exit (which never runs deferred functions) with NO
// chance to reconcile its budget reservation, record telemetry, or write
// its audit-log line — not just an unbilled request, but a total
// accounting/audit blackout, on every routine deploy that happens to
// catch a request mid-stream. Total worst-case shutdown time is
// gracefulShutdownTimeout + postShutdownDrainGrace (45s at these
// defaults) — a request still running past that point is still
// force-killed (a genuinely stuck request, e.g. no context deadline
// against a hung upstream, was never going to bill correctly regardless
// of how long it's given).
//
// **Changed 2026-09-17**: a package-level var for the same
// test-overridability reason as gracefulShutdownTimeout above.
var postShutdownDrainGrace = 15 * time.Second

// trackInFlight wraps next so wg.Add/Done bracket every request the
// returned handler serves — used only on the client-facing mux, never
// the admin mux (admin writes are already short/synchronous, with
// nothing analogous to a long-running streamed request's own deferred
// finalize to protect). Applied around wrapHTTPServerSpan's own span
// creation (Handler: trackInFlight(wg, wrapHTTPServerSpan(mux)) at this
// function's call site), so a request drained by drainInFlight below
// still gets a properly-closed span, not one abandoned mid-flight.
func trackInFlight(wg *sync.WaitGroup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.Add(1)
		defer wg.Done()
		next.ServeHTTP(w, r)
	})
}

// drainInFlight waits up to grace for every handler goroutine wg is
// tracking to finish on its own — giving each one, including its own
// deferred dataplane.Pipeline.finalize call, a real chance to complete
// after http.Server.Shutdown's own wait has already given up (see
// gracefulShutdownTimeout/postShutdownDrainGrace's own doc comments for
// why Shutdown's deadline expiring does NOT itself stop a still-running
// handler). Logs a warning and returns promptly, rather than blocking
// forever, if grace elapses with work still outstanding — a genuinely
// stuck request is accepted as still force-killed by the caller's
// subsequent os.Exit, not something this function can safely wait out
// indefinitely.
func drainInFlight(wg *sync.WaitGroup, grace time.Duration, logger *slog.Logger) {
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(grace):
		logger.Warn("gateway_shutdown_forced_with_requests_still_in_flight")
	}
}

// maxRequestBodyBytes bounds a single /v1/chat/completions request body —
// large enough for a real multi-modal (inline base64 image/document)
// request, small enough to bound worst-case per-request memory. Per
// docs/rfcs/2026-09-06-gateway-multimodal-content.md's own named,
// previously-open gap (THREAT_MODEL.md's Gateway DoS row): before this,
// io.ReadAll(r.Body) had no size limit anywhere in this codebase.
const maxRequestBodyBytes = 32 << 20 // 32MiB

// defaultAdminListenAddr is used when cfg.Admin.TokenEnv is set but
// cfg.Admin.ListenAddr is left empty — loopback-only, never a wildcard
// address, per docs/rfcs/2026-09-05-gateway-admin-api.md's "never
// internet-facing by default" rule: reaching the admin surface from
// outside the host requires a deliberate, explicit listen_addr choice.
const defaultAdminListenAddr = "127.0.0.1:8081"

// streamIdleTimeout bounds dataplane.NewHTTPUpstreamStreamCaller's idle
// window — see that function's own doc comment for why this is an IDLE
// bound (reset on every real byte of progress), not a single deadline
// over the whole streamed call the way the non-streaming caller's
// upstreamHTTPTimeout is. Reuses the exact same 60s magnitude as
// upstreamHTTPTimeout deliberately, for consistency with the one other
// upstream-call bound this codebase has — not because the two mean the
// same thing (they don't: one is whole-call, one is per-idle-gap) but
// because there is no evidence either direction (shorter or longer) is
// actually warranted yet. A fixed default, not a config field, matching
// upstreamHTTPTimeout's own precedent of staying an internal constant
// rather than a new YAML knob for this pass.
const streamIdleTimeout = 60 * time.Second

// upstreamHTTPTimeout bounds NewHTTPUpstreamCaller's whole non-streaming
// call (connect + headers + full buffered body) via http.Client.Timeout —
// correct for that path since a non-streaming response is never
// legitimately long-lived. Named here so streamIdleTimeout's own doc
// comment above has a real symbol to point at for the contrast, rather
// than a bare "60 * time.Second" repeated with no link between the two.
const upstreamHTTPTimeout = 60 * time.Second

// upstreamMaxIdleConnsPerHost raises Go stdlib's default (2, see
// http.DefaultMaxIdleConnsPerHost) for both upstream http.Clients below.
// Go's own net/http.Transport doc comment instructs Transports/Clients
// to be reused instead of created per request BECAUSE they cache
// connections internally -- Kelvran already does that correctly (both
// clients below are built once, here, and shared across every
// concurrent request-handling goroutine, per docs/upgrade-research/
// performance-latency-optimization-2026-09-14.md's own confirmation).
// But once concurrently-open connections to one upstream host exceed
// this cap, Go closes the excess immediately after each request
// completes rather than keeping them warm for any grace period
// (net/http/transport.go's tryPutIdleConn/putOrCloseIdleConn), which
// under a concurrent burst to the same provider host causes real
// connection churn and, at worst, ephemeral-port exhaustion
// (golang/go#13801) -- a documented operational failure mode, not a
// theoretical inefficiency. 100 is a deliberately generous, round
// default given Kelvran has no production traffic yet to calibrate a
// tighter number against; revisit once real per-host concurrency data
// exists (e.g. via a future pprof-driven pass).
const upstreamMaxIdleConnsPerHost = 100

// overheadDurationHeader mirrors LiteLLM's own shipped
// x-litellm-overhead-duration-ms header (confirmed real, standing
// production convention, not a proposal) -- reports how much of a
// buffered chat-completion request's total wall-clock time was
// Kelvran's own added latency, isolated from the real upstream
// provider round-trip. Buffered path only -- see this header's own
// wiring at chatCompletionsHandler's call site, and
// docs/rfcs/2026-09-14-gateway-overhead-duration-header.md for why the
// streaming path cannot set it. Per
// docs/upgrade-research/load-testing-capacity-planning-2026-09-14.md
// Finding 4.
const overheadDurationHeader = "X-Kelvran-Overhead-Duration-Ms"

// newUpstreamTransport builds the http.Transport shared by both upstream
// http.Clients below -- one *http.Transport instance, passed to both,
// which is safe (Transport is safe for concurrent use by multiple
// goroutines and multiple Clients) and lets a streaming and a
// non-streaming call to the same upstream host reuse the same pooled
// idle connections. Cloned from http.DefaultTransport, not a bare
// &http.Transport{}, so every other stdlib default (dial timeouts, TLS
// handshake timeout, HTTP/2 support) is preserved -- only the per-host
// idle-connection cap is deliberately overridden, per
// upstreamMaxIdleConnsPerHost's own doc comment.
func newUpstreamTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = upstreamMaxIdleConnsPerHost
	return t
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to the gateway's YAML config file")
	validateOnly := flag.Bool("validate", false, "load and validate the config file, then exit (0 if valid, 1 if not) -- no listener, no store, no telemetry, nothing else started")
	flag.Parse()

	if *validateOnly {
		cfg, err := controlplane.Load(*configPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(1)
		}
		if err := validateConfig(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(1)
		}
		fmt.Println("config is valid")
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Go has no native cgroup-memory-aware GOMEMLIMIT equivalent (unlike
	// GOMAXPROCS, which Go 1.25+ already sets container-aware, but only
	// when a real CPU *limit*, not just a request, is configured) --
	// exceeding a Kubernetes memory *limit* triggers a hard OOM-kill,
	// while exceeding a CPU limit only throttles, making memory
	// misconfiguration the higher-severity risk for this streaming Go
	// binary. Explicit call (not the package's own convenience blank-
	// import, which would run in init(), before slog.SetDefault above,
	// and log via the stdlib default handler instead of this process's
	// real JSON one) so the ratio/logger are both deliberate, not
	// implicit. A genuine no-cgroup-memory-limit environment (a bare
	// `docker run`/local dev/CI) is handled internally by Set itself
	// (sets GOMEMLIMIT to math.MaxInt64, returns a nil error) -- any
	// error this call DOES return is a real failure (e.g. a malformed
	// AUTOMEMLIMIT env var), worth a log line, never worth failing
	// startup over. See docs/upgrade-research/kubernetes-production-
	// deployment-2026-09-14.md Finding 5.
	if _, err := memlimit.Set(memlimit.WithLogger(logger), memlimit.WithRatio(0.9)); err != nil {
		logger.Warn("automemlimit: could not set GOMEMLIMIT from cgroup", "error", err)
	}

	if err := run(*configPath, logger); err != nil {
		logger.Error("gateway exited", "error", err)
		os.Exit(1)
	}
}

// namedServer pairs a *http.Server with a human-readable name, purely so
// shutdownServersConcurrently's own returned error can say WHICH server
// failed to drain — see that function's own doc comment for why this
// matters. The name is never used for anything else (no lookup, no
// routing) — it exists only to be embedded in an error string.
type namedServer struct {
	name string
	srv  *http.Server
}

// shutdownServersConcurrently calls Shutdown on every server in servers
// whose srv is non-nil, CONCURRENTLY, all against the same ctx -- so a
// slow drain on one server never starves another's share of that same
// deadline.
//
// Corrected, a real bug an audit found: shutdownBoth previously called
// server.Shutdown(shutdownCtx) and adminServer.Shutdown(shutdownCtx)
// SEQUENTIALLY. If the main (client-facing) server's own Shutdown
// consumed the whole gracefulShutdownTimeout waiting on a slow drain
// (e.g. an SSE stream near its own deadline), the admin server's
// Shutdown was then entered with an already-expired context and
// returned context.DeadlineExceeded almost immediately -- giving an
// in-flight admin mutation (POST /admin/backup, POST
// /admin/virtual_keys/{name}) effectively zero grace before the
// following os.Exit killed it mid-flight, with no warning specific to
// that request, since drainInFlight only tracks the client-facing
// mux's own inFlight WaitGroup, never the admin one (trackInFlight's
// own doc comment: "used only on the client-facing mux, never the
// admin mux"). Extracted into its own function, rather than left as an
// inline closure, specifically so this concurrency property is
// directly unit-testable against real *http.Server instances.
//
// Corrected again, a second real gap the same audit found: net/http's
// own Server.Shutdown returns only the bare context error (e.g.
// context.DeadlineExceeded, a fixed, unattributed error value) with no
// listener/address identity baked in -- when both servers time out
// against the shared deadline concurrently, the plain errors.Join
// result used to read as two textually IDENTICAL "context deadline
// exceeded" lines, giving an operator reading the "gateway exited" log
// line no way to tell whether the client server, the admin server, or
// both actually failed to drain. Each per-server error is now wrapped
// with its own name before joining, so the log line names exactly which
// server(s) failed.
func shutdownServersConcurrently(ctx context.Context, servers ...namedServer) error {
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, ns := range servers {
		if ns.srv == nil {
			continue
		}
		wg.Add(1)
		go func(i int, ns namedServer) {
			defer wg.Done()
			if err := ns.srv.Shutdown(ctx); err != nil {
				errs[i] = fmt.Errorf("%s: %w", ns.name, err)
			}
		}(i, ns)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := controlplane.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Init is a process-startup concern, called before buildPipeline (not
	// inside it) — buildPipeline's signature and behavior stay unchanged
	// so every integration-test helper that calls it directly keeps
	// working with the SDK's no-op default tracer, per
	// docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md.
	shutdown, err := telemetry.Init(context.Background(), telemetry.Config{
		Exporter:     cfg.Telemetry.Exporter,
		OTLPEndpoint: cfg.Telemetry.OTLPEndpoint,
	})
	if err != nil {
		return fmt.Errorf("initializing telemetry: %w", err)
	}
	// Operator-visible at startup, per
	// docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md: this
	// process's own InstanceID is exactly what the cache_cross_instance_check
	// log lines (gateway/internal/gateway/dataplane) and every
	// kelvran.cache.l3.gate_outcome metric data point now carry, so an
	// operator can confirm it once here rather than only inferring it
	// from later request-level output.
	logger.Info("gateway_starting", "instance_id", telemetry.InstanceID)
	// Real as of 2026-09-05 (previously best-effort only, a gap this
	// RFC's own Drawbacks section named): flushes on both a clean
	// ListenAndServe error return AND a real SIGTERM/SIGINT, since
	// server.Shutdown below now always returns before this defer runs,
	// on every exit path.
	defer func() { _ = shutdown(context.Background()) }()

	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		return fmt.Errorf("building pipeline: %w", err)
	}
	// Real on every exit path as of 2026-09-05, same reasoning as the
	// telemetry shutdown above. Every budget update is already durably
	// persisted synchronously by Record itself
	// (docs/rfcs/2026-09-03-budget-persistence.md), so this Close is
	// about releasing the bbolt file's exclusive lock cleanly, not about
	// flushing unwritten data.
	defer func() { _ = pipeline.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))
	mux.HandleFunc("/v1/embeddings", embeddingsHandler(pipeline))
	mux.HandleFunc("/healthz", healthzHandler)

	// inFlight tracks real client-facing handler invocations so shutdown
	// can give them a fair, bounded chance to finish — including their
	// own deferred finalize call — before a hard os.Exit, per
	// postShutdownDrainGrace's own doc comment.
	var inFlight sync.WaitGroup
	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: trackInFlight(&inFlight, wrapHTTPServerSpan(mux)),
		// ReadHeaderTimeout: an unset value leaves this server open to a
		// real Slowloris attack (a client trickling request headers in
		// to hold a connection slot open indefinitely), caught by gosec.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// adminServer is nil unless cfg.Admin.TokenEnv is set — the whole
	// admin surface is off by default, per
	// docs/rfcs/2026-09-05-gateway-admin-api.md, matching every other
	// optional subsystem's convention. A separate *http.Server on its own
	// listener, never the same mux/port as the client-facing server
	// above — see that RFC's "never internet-facing by default" section.
	var adminServer *http.Server
	if cfg.Admin.TokenEnv != "" {
		adminToken := os.Getenv(cfg.Admin.TokenEnv)
		if adminToken == "" {
			return fmt.Errorf("admin.token_env %q is set but resolves to an empty environment variable — refusing to start an unauthenticated admin server", cfg.Admin.TokenEnv)
		}
		var viewerToken string
		if cfg.Admin.ViewerTokenEnv != "" {
			viewerToken = os.Getenv(cfg.Admin.ViewerTokenEnv)
			if viewerToken == "" {
				return fmt.Errorf("admin.viewer_token_env %q is set but resolves to an empty environment variable — refusing to start an unauthenticated viewer tier", cfg.Admin.ViewerTokenEnv)
			}
		}
		var costViewerToken string
		if cfg.Admin.CostViewerTokenEnv != "" {
			costViewerToken = os.Getenv(cfg.Admin.CostViewerTokenEnv)
			if costViewerToken == "" {
				return fmt.Errorf("admin.cost_viewer_token_env %q is set but resolves to an empty environment variable — refusing to start an unauthenticated cost-viewer tier", cfg.Admin.CostViewerTokenEnv)
			}
		}
		var operatorToken string
		if cfg.Admin.OperatorTokenEnv != "" {
			operatorToken = os.Getenv(cfg.Admin.OperatorTokenEnv)
			if operatorToken == "" {
				return fmt.Errorf("admin.operator_token_env %q is set but resolves to an empty environment variable — refusing to start an unauthenticated operator tier", cfg.Admin.OperatorTokenEnv)
			}
		}
		adminListenAddr := cfg.Admin.ListenAddr
		if adminListenAddr == "" {
			adminListenAddr = defaultAdminListenAddr
		}
		adminServer = &http.Server{
			Addr:              adminListenAddr,
			Handler:           admin.Handler(cfg, pipeline, admin.Credentials{Admin: adminToken, Viewer: viewerToken, CostViewer: costViewerToken, Operator: operatorToken}, logger),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	// ctx is canceled the moment a real SIGTERM/SIGINT arrives. stop
	// un-registers the signal handler once this function is done reacting
	// to one, restoring Go's own default (exit) behavior for anything
	// received afterward — including a second, impatient signal during
	// the graceful drain below, which should still be able to force-kill
	// the process rather than being silently absorbed forever.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Active/synthetic health-probing, per
	// docs/rfcs/2026-09-07-gateway-active-health-probing.md — a no-op
	// (RunHealthProbeLoop returns immediately) unless health_probe.
	// interval_seconds is configured. Scoped to ctx, exactly like the
	// main/admin servers below: canceled by the same SIGTERM/SIGINT that
	// triggers graceful shutdown, no separate stop mechanism needed.
	go pipeline.RunHealthProbeLoop(ctx, time.Duration(cfg.HealthProbe.IntervalSeconds)*time.Second)

	// Config-propagation subscriber, per internal/configpropagation's
	// own doc comment — a no-op unless config_propagation.redis_addr is
	// configured. A SEPARATE *redis.Client connection from the one
	// buildPipeline's own newConfigPublisher opened above (see that
	// function's own doc comment for why) — Subscribe blocks until ctx
	// is canceled, exactly like RunHealthProbeLoop, so it needs no
	// separate stop mechanism either.
	if cfg.ConfigPropagation.RedisAddr != "" {
		subscriber := configpropagation.Open(cfg.ConfigPropagation.RedisAddr)
		go func() {
			err := subscriber.Subscribe(ctx, func(event configpropagation.MutationEvent) {
				if event.OriginInstanceID == telemetry.InstanceID {
					// This instance's own mutation, already applied
					// locally before it was ever published — re-applying
					// it here would be a redundant, pointless no-op at
					// best (SetWeight is idempotent for the same value)
					// and a wasted log line at worst.
					return
				}
				switch event.Type {
				case configpropagation.TypeDeploymentWeight:
					var payload configpropagation.DeploymentWeightPayload
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						logger.Warn("configpropagation_payload_unmarshal_failed", "type", event.Type, "error", err)
						return
					}
					if err := pipeline.ApplyDeploymentWeightFromEvent(payload.Model, payload.DeploymentName, payload.Weight, event.PublishedAtUnixNano); err != nil {
						logger.Warn("configpropagation_apply_failed", "type", event.Type, "deployment", payload.DeploymentName, "error", err)
					}
				default:
					// Forward-compatible: an event type this build
					// doesn't know about yet (e.g. published by a newer
					// gateway version during a rolling deploy) is
					// skipped, never treated as a fatal error.
				}
			})
			if err != nil && ctx.Err() == nil {
				logger.Warn("configpropagation_subscribe_stopped", "error", err)
				telemetry.RecordConfigPropagationSubscribeStopped(ctx)
			}
		}()
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", cfg.ListenAddr)
		serveErr <- server.ListenAndServe()
	}()

	// adminServeErr is never sent to when adminServer is nil — a select
	// on it simply never fires, so it costs nothing to always include.
	adminServeErr := make(chan error, 1)
	if adminServer != nil {
		go func() {
			logger.Info("admin server listening", "addr", adminServer.Addr)
			adminServeErr <- adminServer.ListenAndServe()
		}()
	}

	// shutdownBoth drains the main server, and the admin server too if
	// one was started, within one shared deadline via
	// shutdownServersConcurrently below. Safe to call on adminServer
	// even if its own ListenAndServe already returned (Shutdown on an
	// unserved/already-stopped *http.Server is a no-op).
	shutdownBoth := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		return shutdownServersConcurrently(shutdownCtx,
			namedServer{name: "client server", srv: server},
			namedServer{name: "admin server", srv: adminServer},
		)
	}

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = shutdownBoth()
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case err := <-adminServeErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = shutdownBoth()
			return fmt.Errorf("admin http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		stop()
		logger.Info("gateway shutting down", "reason", context.Cause(ctx))
		shutdownErr := shutdownBoth()
		// Give any still-running request-handler goroutine (one
		// Shutdown's own wait gave up on above, per
		// gracefulShutdownTimeout's doc comment) a real, bounded chance
		// to finish on its own — including its deferred finalize call —
		// before this function returns and main()'s os.Exit runs, which
		// would otherwise kill it mid-flight with zero cleanup. Runs
		// regardless of shutdownErr: a Shutdown timeout is exactly the
		// case this exists for.
		drainInFlight(&inFlight, postShutdownDrainGrace, logger)
		if shutdownErr != nil {
			return fmt.Errorf("graceful shutdown: %w", shutdownErr)
		}
		return nil
	}
}

// wrapHTTPServerSpan adds a generic HTTP-server-level span (via otelhttp)
// around every request the server handles, nesting the existing
// per-request GenAI span (started inside chatCompletionsHandler) as a
// child of it — the future addition docs/rfcs/2026-09-02-otel-tracing-
// agent-run-id.md named and deliberately deferred ("one span per request,
// not a full HTTP-server-level span nested around it"). Uses the global
// TracerProvider/TextMapPropagator telemetry.Init already installed —
// no explicit otelhttp.With* option needed, matching this codebase's own
// established no-manual-provider-threading convention (see
// internal/telemetry's package-level Tracer var).
func wrapHTTPServerSpan(handler http.Handler) http.Handler {
	return otelhttp.NewHandler(handler, "gateway.http")
}

// defaultCacheJitterFraction is the real production default (10%) when an
// operator's cache.jitter_fraction/l2.jitter_fraction/l3.jitter_fraction
// is absent or <= 0 — mirroring inprocess.Cache's own identical constant,
// per docs/rfcs/2026-09-10-gateway-cache-ttl-jitter.md. Resolving a
// config-driven zero into the real default is this package's job, not
// inprocess's: NewWithClockAndJitter always jitters by exactly
// rand()*jitterFraction*ttl, whatever jitterFraction it's given, with no
// "<=0 means default" special-casing of its own (that resolution only
// matters for an operator's YAML field, which can't distinguish "not
// set" from an explicit zero either way).
const defaultCacheJitterFraction = 0.10

// resolveJitterFraction turns a config-driven jitter fraction (0 or
// unset, indistinguishable) into the real default.
func resolveJitterFraction(f float64) float64 {
	if f <= 0 {
		return defaultCacheJitterFraction
	}
	return f
}

// buildPipeline resolves every secret referenced by name in cfg from the
// environment and wires the full dataplane.Pipeline.
func buildPipeline(cfg *controlplane.Config, logger *slog.Logger) (*dataplane.Pipeline, error) {
	// Config-only checks first, before opening any bbolt store below (a
	// strict improvement over this check's own prior position, deep
	// inside the deployment loop below, after identityStore was already
	// opened) — see validateConfig's own doc comment.
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	virtualKeys := make([]identity.VirtualKey, 0, len(cfg.VirtualKeys))
	keyConfigs := make([]ratelimit.KeyConfig, 0, len(cfg.VirtualKeys))
	// concurrencyConfigs feeds ratelimit.NewConcurrencyLimiter, per
	// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design
	// (b) — built alongside keyConfigs above, from the same per-key loop,
	// rather than a second pass over cfg.VirtualKeys.
	concurrencyConfigs := make([]ratelimit.ConcurrencyConfig, 0, len(cfg.VirtualKeys))
	for _, vk := range cfg.VirtualKeys {
		burst, refill := ratelimit.ResolveKeyRateLimit(vk.RateLimitBurst, vk.RateLimitRefill)
		var allowedModels map[string]struct{}
		if len(vk.AllowedModels) > 0 {
			allowedModels = make(map[string]struct{}, len(vk.AllowedModels))
			for _, m := range vk.AllowedModels {
				allowedModels[m] = struct{}{}
			}
		}
		var allowedRegions map[string]struct{}
		if len(vk.AllowedRegions) > 0 {
			allowedRegions = make(map[string]struct{}, len(vk.AllowedRegions))
			for _, r := range vk.AllowedRegions {
				allowedRegions[r] = struct{}{}
			}
		}
		virtualKeys = append(virtualKeys, identity.VirtualKey{
			ID:                    vk.Name,
			KeyHash:               vk.KeyHash,
			BudgetUSD:             vk.BudgetUSD,
			BudgetResetInterval:   time.Duration(vk.BudgetResetIntervalSeconds) * time.Second,
			BudgetWarnPercent:     vk.BudgetWarnPercent,
			AllowedModels:         allowedModels,
			AllowedRegions:        allowedRegions,
			RateLimitBurst:        burst,
			RateLimitRefill:       refill,
			MaxConcurrentRequests: vk.MaxConcurrentRequests,
			BillingSubjectID:      vk.BillingSubjectID,
		})
		if vk.TPMCapacity > 0 && cfg.RateLimit.RedisAddr != "" {
			logger.Warn("virtual key configures a TPM rate limit, but Redis rate-limit mode is active; TPM is in-memory-only in v1 and will not be enforced",
				"key", vk.Name)
		}
		var perModel map[string]ratelimit.ModelRateLimit
		if len(vk.PerModelRateLimits) > 0 {
			perModel = make(map[string]ratelimit.ModelRateLimit, len(vk.PerModelRateLimits))
			for model, mrl := range vk.PerModelRateLimits {
				perModel[model] = ratelimit.ModelRateLimit{
					Capacity:           mrl.Burst,
					RefillPerSecond:    mrl.RefillPerSecond,
					TPMCapacity:        mrl.TPMCapacity,
					TPMRefillPerSecond: mrl.TPMRefillPerSecond,
				}
			}
		}
		keyConfigs = append(keyConfigs, ratelimit.KeyConfig{
			ID:                 vk.Name,
			Capacity:           burst,
			RefillPerSecond:    refill,
			TPMCapacity:        vk.TPMCapacity,
			TPMRefillPerSecond: vk.TPMRefillPerSecond,
			PerModel:           perModel,
		})
		concurrencyConfigs = append(concurrencyConfigs, ratelimit.ConcurrencyConfig{
			ID:          vk.Name,
			MaxInFlight: vk.MaxConcurrentRequests,
		})
	}
	// identityStore is nil unless cfg.Admin.PersistPath is set — see
	// dataplane.Config.IdentityStore's own doc comment. Opened here,
	// before identity.NewVerifier, so a persisted virtual key's own
	// overlay (mergePersistedVirtualKeys) is part of the FIRST Verifier
	// this process ever constructs, not something that only takes effect
	// after the first live admin mutation.
	var identityStore identity.Store
	if cfg.Admin.PersistPath != "" {
		store, err := openPersistStoreWithRecovery(cfg.Admin.PersistPath, cfg.Admin.OnCorruptStore, logger, identityboltstore.Open)
		if err != nil {
			return nil, fmt.Errorf("opening virtual-key store at %q: %w", cfg.Admin.PersistPath, err)
		}
		identityStore = store
		virtualKeys, keyConfigs, concurrencyConfigs, err = mergePersistedVirtualKeys(virtualKeys, keyConfigs, concurrencyConfigs, store, logger)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("hydrating virtual keys from %q: %w", cfg.Admin.PersistPath, err)
		}
	}

	verifier, err := identity.NewVerifier(virtualKeys)
	if err != nil {
		return nil, fmt.Errorf("constructing identity verifier: %w", err)
	}

	registry := newAdapterRegistry()

	deployments := make([]dataplane.Deployment, 0, len(cfg.Deployments))
	routerDeployments := make([]router.Deployment, 0, len(cfg.Deployments))
	// deploymentConcurrencyConfigs/deploymentRateLimitConfigs feed
	// ratelimit.NewConcurrencyLimiter/ratelimit.NewInMemoryKeyLimiter for
	// the deployment-scoped (not per-key) ceilings, per docs/upgrade-
	// research/gateway-per-deployment-concurrency-2026-09-09.md — built
	// alongside deployments/routerDeployments above, from the same
	// per-deployment loop. Always in-memory in v1, regardless of
	// cfg.RateLimit.RedisAddr (which only governs the per-KEY limiter,
	// via newKeyLimiter below) — a distributed backend is named future
	// work, not built here, matching the same deferral this codebase
	// already applies to the per-key ConcurrencyLimiter.
	deploymentConcurrencyConfigs := make([]ratelimit.ConcurrencyConfig, 0, len(cfg.Deployments))
	deploymentRateLimitConfigs := make([]ratelimit.KeyConfig, 0, len(cfg.Deployments))
	for _, d := range cfg.Deployments {
		// Provider-registration and fallback_chains referential-integrity
		// are both already checked above, by validateConfig — see that
		// function's own doc comment. Nothing else here needs re-checking.
		dep := dataplane.Deployment{
			Name:                            d.Name,
			Model:                           d.Model,
			Provider:                        d.Provider,
			UpstreamModel:                   d.UpstreamModel,
			BaseURL:                         d.BaseURL,
			Region:                          d.Region,
			FallbackChains:                  d.FallbackChains,
			DisableCacheControlAutoPopulate: d.DisableCacheControlAutoPopulate,
			SharedAcrossTenants:             d.SharedAcrossTenants,
			Kind:                            d.Kind,
		}
		if d.Provider == "bedrock" {
			dep.AccessKeyID = os.Getenv(d.AccessKeyIDEnv)
			if dep.AccessKeyID == "" {
				logger.Warn("deployment's AWS access key ID env var is not set; calls to this deployment will fail",
					"deployment", d.Name, "env_var", d.AccessKeyIDEnv)
			}
			dep.SecretAccessKey = os.Getenv(d.SecretAccessKeyEnv)
			if dep.SecretAccessKey == "" {
				logger.Warn("deployment's AWS secret access key env var is not set; calls to this deployment will fail",
					"deployment", d.Name, "env_var", d.SecretAccessKeyEnv)
			}
			if d.SessionTokenEnv != "" {
				dep.SessionToken = os.Getenv(d.SessionTokenEnv)
			}
		} else {
			dep.APIKey = os.Getenv(d.APIKeyEnv)
			if dep.APIKey == "" {
				logger.Warn("deployment's upstream API key env var is not set; calls to this deployment will fail",
					"deployment", d.Name, "env_var", d.APIKeyEnv)
			}
		}
		deployments = append(deployments, dep)
		routerDeployments = append(routerDeployments, router.Deployment{
			Name:     d.Name,
			Model:    d.Model,
			Weight:   d.Weight,
			CostTier: d.CostTier,
			Sticky:   d.Sticky,
		})
		deploymentConcurrencyConfigs = append(deploymentConcurrencyConfigs, ratelimit.ConcurrencyConfig{
			ID:          d.Name,
			MaxInFlight: d.MaxConcurrentRequests,
		})
		deploymentRateLimitConfigs = append(deploymentRateLimitConfigs, ratelimit.KeyConfig{
			ID:              d.Name,
			Capacity:        d.RateLimitBurst,
			RefillPerSecond: d.RateLimitRefill,
		})
	}

	depRouter := router.New(routerDeployments, router.HealthConfig{
		UnhealthyThreshold:         cfg.HealthProbe.UnhealthyThreshold,
		HealthyThreshold:           cfg.HealthProbe.HealthyThreshold,
		RecoveryRampSteps:          cfg.HealthProbe.RecoveryRampSteps,
		RecoveryRampInitialPercent: cfg.HealthProbe.RecoveryRampInitialPercent,
	})

	priceTable := costaccounting.PriceTable{}
	for model, price := range cfg.PriceTable {
		priceTable[model] = costaccounting.ModelPrice{
			PromptPerToken:        price.PromptPerToken,
			CompletionPerToken:    price.CompletionPerToken,
			CacheReadPerToken:     price.CacheReadPerToken,
			CacheCreationPerToken: price.CacheCreationPerToken,
		}
	}

	budgetTracker, err := newBudgetTracker(cfg.Budget, cfg.Admin.OnCorruptStore, logger)
	if err != nil {
		return nil, fmt.Errorf("constructing budget tracker: %w", err)
	}

	promptStore, err := newPromptStore(cfg.Prompt, cfg.Admin.OnCorruptStore, logger)
	if err != nil {
		return nil, fmt.Errorf("constructing prompt store: %w", err)
	}

	keyLimiter, err := newKeyLimiter(cfg.RateLimit, keyConfigs)
	if err != nil {
		return nil, fmt.Errorf("constructing rate limiter: %w", err)
	}

	// A SEPARATE *redis.Client connection from the one run()'s own
	// subscriber goroutine opens below (see newConfigPublisher's own
	// doc comment for why) -- both lightweight, both fail-open on an
	// unreachable address exactly like newKeyLimiter's Redis backend.
	configPublisher := newConfigPublisher(cfg.ConfigPropagation)
	alertNotifier := newAlertNotifier(cfg.Alerting, logger)

	upstreamTransport := newUpstreamTransport()

	guardrailEngine := newGuardrailEngine(cfg.Guardrails, logger)

	return dataplane.NewPipeline(dataplane.Config{
		Verifier:      verifier,
		IdentityStore: identityStore,
		// Always constructed, never nil — mirrors Cache/CacheL2/CacheL3's
		// own "no config gate, just always wire it in" precedent below:
		// idempotency.Store's own contract already makes an empty
		// Idempotency-Key header (the overwhelmingly common case) a
		// guaranteed no-op, so there is no "is anything configured" gate
		// to add here. Single-instance-only (see
		// internal/idempotency/inprocess's own package doc) until a
		// Redis-backed implementation exists for the gateway2 multi-
		// instance Compose profile — named, not yet built.
		IdempotencyStore: idempotencyinprocess.New(),
		Prompts:          promptStore,
		Limiter:          keyLimiter,
		// Always constructed, never nil — a virtual key with
		// MaxConcurrentRequests <= 0 (every config written before this
		// feature existed) is simply absent from concurrencyConfigs'
		// resulting limits map, per
		// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design
		// (b), so this is safe to always wire in rather than needing its
		// own "is anything configured" gate the way e.g. Redis rate-
		// limiting does.
		Concurrency: ratelimit.NewConcurrencyLimiter(concurrencyConfigs),
		// DeploymentConcurrency/DeploymentLimiter are the deployment-scoped
		// (never per-key) ceilings, per docs/upgrade-research/gateway-per-
		// deployment-concurrency-2026-09-09.md — always constructed, never
		// nil, mirroring Concurrency's own "absent means unlimited for that
		// ID" precedent above: a deployment with no rate_limit: section
		// (every config written before this feature existed) is simply
		// absent from deploymentRateLimitConfigs/HasLimit, and
		// MaxConcurrentRequests <= 0 is absent from
		// deploymentConcurrencyConfigs' resulting limits map, so this is
		// safe to always wire in.
		DeploymentConcurrency: ratelimit.NewConcurrencyLimiter(deploymentConcurrencyConfigs),
		DeploymentLimiter:     ratelimit.NewInMemoryKeyLimiter(deploymentRateLimitConfigs),
		Budget:                budgetTracker,
		Cache:                 inprocess.NewWithClockAndJitter(cfg.Cache.MaxEntries, time.Now, resolveJitterFraction(cfg.Cache.JitterFraction), rand.Float64),
		CacheL2:               inprocess.NewWithClockAndJitter(cfg.Cache.L2.MaxEntries, time.Now, resolveJitterFraction(cfg.Cache.L2.JitterFraction), rand.Float64),
		CacheL3:               inprocess.NewLexicalCacheWithClockAndJitter(cfg.Cache.L3.MaxEntries, time.Now, resolveJitterFraction(cfg.Cache.L3.JitterFraction), rand.Float64),
		Guardrails:            guardrailEngine,
		Adapters:              registry,
		Router:                depRouter,
		Deployments:           deployments,
		CostCalculator:        costaccounting.NewCalculator(priceTable),
		Upstream:              dataplane.NewHTTPUpstreamCaller(&http.Client{Timeout: upstreamHTTPTimeout, Transport: upstreamTransport}),
		EmbeddingUpstream:     dataplane.NewHTTPEmbeddingUpstreamCaller(&http.Client{Timeout: upstreamHTTPTimeout, Transport: upstreamTransport}),
		ConfigPublisher:       configPublisher,
		AlertNotifier:         alertNotifier,
		// Streaming upstream calls deliberately do NOT use client.Timeout
		// (the field above) — that would kill a long-running-but-healthy
		// stream mid-way, exactly as readily as a genuinely stalled one.
		// The &http.Client{} passed here stays a zero-value-Timeout
		// client on purpose: NewHTTPUpstreamStreamCaller enforces its own
		// bound via streamIdleTimeout instead — an idle window, reset on
		// every real byte of progress, that closes the real gap found by
		// evals/tests/fixtures/regression_corpus_routing_chaos.json's
		// "chaos-streaming-no-upstream-timeout-gap" case (a stalled
		// streaming upstream used to hang indefinitely, bounded only by
		// the original inbound client disconnecting). See that function's
		// own doc comment for the full design rationale.
		UpstreamStream: dataplane.NewHTTPUpstreamStreamCaller(&http.Client{Transport: upstreamTransport}, streamIdleTimeout),
		Logger:         logger,
		CacheTTL:       time.Duration(cfg.Cache.TTLSeconds) * time.Second,
		CacheL2TTL:     time.Duration(cfg.Cache.L2.TTLSeconds) * time.Second,
		CacheL3TTL:     time.Duration(cfg.Cache.L3.TTLSeconds) * time.Second,
	})
}

// newAdapterRegistry returns the fixed set of provider adapters this
// gateway ships, in construction (never per-request) order. Shared by
// buildPipeline (the real, wired registry) and validateConfig (which
// only ever checks provider NAME membership) so the two lists can never
// drift apart.
func newAdapterRegistry() adapter.Registry {
	return adapter.Registry{
		"openai":       openai.New(),
		"anthropic":    anthropic.New(),
		"gemini":       gemini.New(),
		"bedrock":      bedrock.New(),
		"openaicompat": openaicompat.New(),
	}
}

// validateConfig runs every startup check that depends ONLY on cfg's own
// content -- never opening a file, never touching the network, never
// reading an environment variable -- so it's safe to call from the
// -validate dry-run flag with no other side effect at all, and safe to
// call as buildPipeline's own first statement, before identityStore ever
// opens a bbolt file (a strict improvement over this check's prior
// position deep inside buildPipeline's deployment loop, after the store
// was already open). Both checks it runs (adapter registration,
// fallback_chains referential integrity) already existed inside
// buildPipeline before this function extracted them; see -validate's own
// flag description in main() and TestValidateConfigNeverOpensAnyBoltStore
// for the actual "no side effects" proof.
func validateConfig(cfg *controlplane.Config) error {
	registry := newAdapterRegistry()
	deployments := make([]dataplane.Deployment, 0, len(cfg.Deployments))
	for _, d := range cfg.Deployments {
		// Fail fast at startup, not per-request, per
		// docs/rfcs/2026-09-05-gateway-gen-ai-provider-name-validation.md
		// — before this check, an unregistered provider produced a
		// generic error only once a real request happened to route to
		// that deployment (dataplane.callDeployment's own "no adapter
		// registered" error), which could sit unnoticed until traffic
		// actually hit it.
		if _, ok := registry[d.Provider]; !ok {
			return fmt.Errorf("deployment %q: no adapter registered for provider %q", d.Name, d.Provider)
		}
		deployments = append(deployments, dataplane.Deployment{
			Name:           d.Name,
			FallbackChains: d.FallbackChains,
		})
	}
	return validateFallbackChainTargets(deployments)
}

// validateFallbackChainTargets fails startup, not first-request, if any
// deployment's fallback_chains names a target deployment that doesn't
// exist — controlplane.Load can't check this itself (it parses one
// deployment's mapping at a time, with no visibility into the full,
// still-being-built deployment set), so this runs here instead, in the
// same spirit as this function's existing "no adapter registered for
// provider" fail-fast check above, per
// docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md.
func validateFallbackChainTargets(deployments []dataplane.Deployment) error {
	names := make(map[string]struct{}, len(deployments))
	for _, d := range deployments {
		names[d.Name] = struct{}{}
	}
	for _, d := range deployments {
		for class, targets := range d.FallbackChains {
			for _, target := range targets {
				if _, ok := names[target]; !ok {
					return fmt.Errorf("deployment %q fallback_chains.%s names %q, which is not a configured deployment", d.Name, class, target)
				}
			}
		}
	}
	return nil
}

// mergePersistedVirtualKeys overlays store's persisted virtual keys onto
// the config-derived virtualKeys/keyConfigs/concurrencyConfigs (built by
// the per-key loop above, all three in the same index order), per
// docs/upgrade-research/admin-operator-experience-2026-09-14.md Finding 2:
// a virtual key ever created or rotated via the live admin API is the
// authoritative version for its ID from then on — config.yaml is only
// ever the BOOTSTRAP/seed state before any admin mutation happens, never
// something that should silently undo a live rotation on every restart.
// A persisted key whose ID isn't in config at all (created purely via the
// admin API) is appended as a net-new entry.
//
// See identity.Store's own doc comment for the real, disclosed scope
// limit this merge inherits: a persisted key's rate limit is rebuilt from
// ONLY its own RateLimitBurst/RateLimitRefill (via the same
// ratelimit.ResolveKeyRateLimit fallback upsertVirtualKeyHandler already
// applies) — any PerModel/TPM override the config version of that same ID
// might have had does not survive, since that shape lives only in
// ratelimit.KeyConfig, never in identity.VirtualKey.
//
// logger records one line per config-declared ID a persisted entry
// overwrites — this is a one-shot startup event (no log-volume concern),
// and a per-ID line is grep-able exactly like every other per-key audit
// line elsewhere in this codebase (e.g. admin_virtual_key_upserted). Never
// logged for a persisted ID with no config-declared counterpart at all
// (the net-new-entry branch below) — there is nothing to disclose an
// override AGAINST in that case.
// openPersistStoreWithRecovery wraps a bbolt-backed store's own Open
// function with cfg.Admin.OnCorruptStore's disclosed choice, per
// docs/upgrade-research/state-durability-operational-recovery-2026-09-15.md
// Finding 4. mode == "fail" (the default) returns open's own error
// completely unchanged -- byte-for-byte the same behavior as before this
// field existed, for every config file that doesn't set it. mode ==
// "reset" is only reached on a genuine open FAILURE (a path that simply
// doesn't exist yet already succeeds on the first open call, since
// bbolt creates it) -- it renames the corrupt file aside to
// "<path>.corrupt-<unix-seconds>" (preserving forensic evidence, never
// deleting it outright, per the finding's own "even a reset shouldn't
// destroy evidence of what went wrong" framing) and retries open ONCE
// against the now-clear path. A rename failure (e.g. a permissions
// issue unrelated to corruption) surfaces the ORIGINAL open error, not
// the rename error -- retrying open against a path that still has the
// corrupt file at it would just reproduce the same failure, so there is
// nothing a second attempt could recover.
//
// Corrected 2026-09-18, a real bug an audit found: this previously
// treated ANY open error as reset-worthy, with zero classification --
// a permissions misconfiguration or a disk-full error during an
// earlier write would have been silently renamed aside and recreated
// EMPTY in "reset" mode, a real data-loss path unrelated to actual
// corruption. isCorruptStoreErr below only matches bbolt's own three
// documented corruption sentinels (ErrInvalid/ErrVersionMismatch/
// ErrChecksum, confirmed returned unwrapped through bolt.Open and
// preserved across every boltstore package's own fmt.Errorf("...: %w",
// err) wrapping) -- any other error (permissions, disk-full, a locked
// file -- though bbolt's own DefaultOptions.Timeout of 0 means a locked
// file blocks forever rather than ever returning an error here) is
// treated exactly like mode == "fail", never reset.
func openPersistStoreWithRecovery[T any](path string, mode string, logger *slog.Logger, open func(string) (T, error)) (T, error) {
	store, err := open(path)
	if err == nil {
		return store, nil
	}
	if mode != "reset" || !isCorruptStoreErr(err) {
		return store, err
	}

	logger.Error("persist_store_open_failed", "path", path, "error", err, "on_corrupt_store", mode)
	backupPath := fmt.Sprintf("%s.corrupt-%d-%d", path, time.Now().Unix(), time.Now().Nanosecond())
	if renameErr := os.Rename(path, backupPath); renameErr != nil {
		logger.Error("persist_store_corrupt_backup_failed", "path", path, "backup_path", backupPath, "error", renameErr)
		return store, err
	}
	logger.Warn("persist_store_reset", "path", path, "backup_path", backupPath)

	store, err = open(path)
	if err != nil {
		return store, fmt.Errorf("re-opening %q after reset: %w", path, err)
	}
	return store, nil
}

// isCorruptStoreErr reports whether err is one of bbolt's own three
// documented on-disk-corruption sentinels, as opposed to any other
// open failure (permissions, disk-full, I/O error) that "reset" mode
// must never treat as corruption -- see openPersistStoreWithRecovery's
// own doc comment for why this classification exists.
func isCorruptStoreErr(err error) bool {
	return errors.Is(err, berrors.ErrInvalid) || errors.Is(err, berrors.ErrVersionMismatch) || errors.Is(err, berrors.ErrChecksum)
}

func mergePersistedVirtualKeys(virtualKeys []identity.VirtualKey, keyConfigs []ratelimit.KeyConfig, concurrencyConfigs []ratelimit.ConcurrencyConfig, store identity.Store, logger *slog.Logger) ([]identity.VirtualKey, []ratelimit.KeyConfig, []ratelimit.ConcurrencyConfig, error) {
	persisted, err := store.Load(context.Background())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading persisted virtual keys: %w", err)
	}

	indexByID := make(map[string]int, len(virtualKeys))
	for i, vk := range virtualKeys {
		indexByID[vk.ID] = i
	}

	for id, vk := range persisted {
		burst, refill := ratelimit.ResolveKeyRateLimit(vk.RateLimitBurst, vk.RateLimitRefill)
		keyConfig := ratelimit.KeyConfig{ID: id, Capacity: burst, RefillPerSecond: refill}
		concurrencyConfig := ratelimit.ConcurrencyConfig{ID: id, MaxInFlight: vk.MaxConcurrentRequests}

		if i, exists := indexByID[id]; exists {
			logger.Info("startup_virtual_key_overridden_by_persisted_store", "key_id", id)
			virtualKeys[i] = vk
			keyConfigs[i] = keyConfig
			concurrencyConfigs[i] = concurrencyConfig
			continue
		}
		virtualKeys = append(virtualKeys, vk)
		keyConfigs = append(keyConfigs, keyConfig)
		concurrencyConfigs = append(concurrencyConfigs, concurrencyConfig)
	}
	return virtualKeys, keyConfigs, concurrencyConfigs, nil
}

// newBudgetTracker constructs a pure in-memory budget.Tracker when
// cfg.PersistPath is empty (the default — a bare config.yaml with no
// budget: section behaves identically to before
// docs/rfcs/2026-09-03-budget-persistence.md existed), or one backed by a
// bbolt store at cfg.PersistPath otherwise — hydrating any existing spend
// immediately, so a restart resumes exactly where it left off.
// onCorruptStore ("fail"/"reset") is forwarded straight to
// openPersistStoreWithRecovery — see that function's own doc comment.
func newBudgetTracker(cfg controlplane.BudgetConfig, onCorruptStore string, logger *slog.Logger) (*budget.Tracker, error) {
	if cfg.PersistPath == "" {
		return budget.NewTracker(), nil
	}
	store, err := openPersistStoreWithRecovery(cfg.PersistPath, onCorruptStore, logger, boltstore.Open)
	if err != nil {
		return nil, fmt.Errorf("opening budget store at %q: %w", cfg.PersistPath, err)
	}
	tracker, err := budget.NewTrackerWithStore(context.Background(), store, logger)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("hydrating budget tracker from %q: %w", cfg.PersistPath, err)
	}
	return tracker, nil
}

// newPromptStore constructs a pure in-memory prompt.Store when
// cfg.PersistPath is empty (the default — identical to prompt.Persister's
// own pre-implementation behavior), or one backed by a bbolt store at
// cfg.PersistPath otherwise — hydrating any existing prompt templates
// immediately, mirroring newBudgetTracker's identical shape, including
// onCorruptStore's identical meaning.
func newPromptStore(cfg controlplane.PromptConfig, onCorruptStore string, logger *slog.Logger) (*prompt.Store, error) {
	if cfg.PersistPath == "" {
		return prompt.NewStore(), nil
	}
	store, err := openPersistStoreWithRecovery(cfg.PersistPath, onCorruptStore, logger, promptboltstore.Open)
	if err != nil {
		return nil, fmt.Errorf("opening prompt store at %q: %w", cfg.PersistPath, err)
	}
	promptStore, err := prompt.NewStoreWithPersister(context.Background(), store)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("hydrating prompt store from %q: %w", cfg.PersistPath, err)
	}
	return promptStore, nil
}

// newKeyLimiter constructs a pure in-memory ratelimit.KeyLimiter when
// cfg.RedisAddr is empty (the default — a bare config.yaml with no
// rate_limit: section behaves identically to before
// docs/rfcs/2026-09-03-distributed-rate-limiting.md existed), or one
// backed by Redis at cfg.RedisAddr otherwise. Opening a redislimiter.Limiter
// never fails on an unreachable address (go-redis dials lazily) — an
// error here means the address itself is malformed, not that Redis is
// currently unavailable, which is exactly the distinction this RFC's
// fail-open policy depends on: gateway startup should not fail-closed on
// Redis being down.
func newKeyLimiter(cfg controlplane.RateLimitConfig, keys []ratelimit.KeyConfig) (*ratelimit.KeyLimiter, error) {
	if cfg.RedisAddr == "" {
		return ratelimit.NewInMemoryKeyLimiter(keys), nil
	}
	backend, err := redislimiter.Open(cfg.RedisAddr)
	if err != nil {
		return nil, fmt.Errorf("opening redis rate limiter at %q: %w", cfg.RedisAddr, err)
	}
	return ratelimit.NewRedisKeyLimiter(keys, backend), nil
}

// newConfigPublisher returns nil (a genuinely nil configpropagation.Publisher
// INTERFACE value, never a non-nil interface wrapping a nil *PubSub
// pointer — the classic Go typed-nil trap dataplane.Pipeline's own
// "p.configPublisher != nil" check depends on being avoided here) when
// cfg.RedisAddr is empty, matching newKeyLimiter's identical "unset
// means the feature doesn't exist" convention. configpropagation.Open
// never fails on an unreachable address (go-redis dials lazily,
// mirroring redislimiter.Open's own identical contract) so this never
// returns an error.
func newConfigPublisher(cfg controlplane.ConfigPropagationConfig) configpropagation.Publisher {
	if cfg.RedisAddr == "" {
		return nil
	}
	return configpropagation.Open(cfg.RedisAddr)
}

// newAlertNotifier builds the optional direct-from-Go webhook push, per
// internal/alerting's own doc comment. Returns a genuine nil interface
// (never a typed-nil-in-interface trap — the same pitfall
// newConfigPublisher's own doc comment already names) when
// cfg.WebhookURLEnv is empty, so dataplane's own `p.alertNotifier !=
// nil` check stays correct. A configured-but-empty-resolved env var
// logs a warning and disables the notifier rather than failing startup
// -- mirrors DeploymentConfig.APIKeyEnv's own "logged as a warning, not
// fatal" convention, since a misdelivered alert is a degraded-signal
// problem, never an auth/security-critical one the way an empty
// AdminConfig.TokenEnv would be.
func newAlertNotifier(cfg controlplane.AlertingConfig, logger *slog.Logger) alerting.Notifier {
	if cfg.WebhookURLEnv == "" {
		return nil
	}
	url := os.Getenv(cfg.WebhookURLEnv)
	if url == "" {
		logger.Warn("alerting_webhook_url_env_unset", "env_var", cfg.WebhookURLEnv)
		return nil
	}
	var signingSecret string
	if cfg.SigningSecretEnv != "" {
		signingSecret = os.Getenv(cfg.SigningSecretEnv)
	}
	return alerting.NewWebhookNotifier(url, signingSecret, logger)
}

// guardrailDefaultPolicyVersion is the operational default when
// cfg.PolicyVersion is empty — controlplane.GuardrailsConfig's own
// zero-value default, applied here since that's an operational default,
// not a config-shape concern that package owns (mirroring how
// TelemetryConfig/CacheL2Config's own defaults are resolved in their
// respective constructors, not in controlplane).
const guardrailDefaultPolicyVersion = "v1"

// newGuardrailEngine builds the Guardrails Engine from cfg, per
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md: the RFC's own
// default detector set and policy, with cfg.CategoryOverrides applied on
// top (both the detection AND detector-error action for that category —
// a single "block"/"warn" knob per category, not two, matching this
// RFC's own "deliberately simple" v1 posture). An unrecognized category
// or action string is logged and skipped, never silently ignored and
// never a fatal startup error — a config typo should not take down the
// gateway, but it must be visible.
func newGuardrailEngine(cfg controlplane.GuardrailsConfig, logger *slog.Logger) *guardrail.Engine {
	version := cfg.PolicyVersion
	if version == "" {
		version = guardrailDefaultPolicyVersion
	}

	policy := guardrail.DefaultPolicy()
	for categoryStr, actionStr := range cfg.CategoryOverrides {
		category := guardrail.Category(categoryStr)
		if _, known := policy.Actions[category]; !known {
			logger.Warn("guardrail_config_unknown_category", "category", categoryStr)
			continue
		}
		var action guardrail.Action
		switch actionStr {
		case "block":
			action = guardrail.ActionBlock
		case "warn":
			action = guardrail.ActionWarn
		default:
			logger.Warn("guardrail_config_unknown_action", "category", categoryStr, "action", actionStr)
			continue
		}
		policy.Actions[category] = action
		policy.ErrorActions[category] = action
	}

	detectors := guardrail.DefaultDetectors()
	if bg := cfg.BedrockGuardrails; bg != nil {
		detectors = append(detectors, bedrockguard.New(bedrockguard.Config{
			Region:           bg.Region,
			AccessKeyID:      os.Getenv(bg.AccessKeyIDEnv),
			SecretAccessKey:  os.Getenv(bg.SecretAccessKeyEnv),
			SessionToken:     envOrEmpty(bg.SessionTokenEnv),
			GuardrailID:      bg.GuardrailID,
			GuardrailVersion: bg.GuardrailVersion,
		}, nil))
		logger.Info("guardrail_bedrock_guardrails_enabled", "guardrail_id", bg.GuardrailID, "guardrail_version", bg.GuardrailVersion)
	}

	return guardrail.NewEngine(detectors, policy, version, logger)
}

// envOrEmpty returns os.Getenv(name), or "" if name itself is empty --
// SessionTokenEnv is optional (only set for temporary/STS credentials),
// unlike AccessKeyIDEnv/SecretAccessKeyEnv which config.go's own parser
// already requires non-empty before BedrockGuardrails is ever non-nil.
func envOrEmpty(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}

// chatCompletionsHandler adapts dataplane.Pipeline.HandleChatCompletion to
// net/http: decode the canonical JSON request body, run the pipeline,
// encode the canonical JSON response (or an appropriate error status).
// healthzHandler is a shallow liveness/readiness probe — it reports only
// that this process is up and serving, never upstream provider
// reachability, per docs/operations/DEPLOY.md's own reasoning: a health
// check that depends on a third-party API being reachable defeats its own
// purpose (a real, healthy gateway would be marked unhealthy by an
// unrelated provider outage). No auth required, matching every standard
// load-balancer/orchestrator health-check convention.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func chatCompletionsHandler(p *dataplane.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}

		var req adapter.ChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}

		// Cheap count-only checks first (Go's own len(), no decode/
		// scan work) so a pathologically-shaped body is rejected before
		// paying for the genuinely expensive per-part/per-tool checks
		// below (base64 decode + MIME sniff; JSON Schema tokenization).
		if err := adapter.ValidateMessageCount(req.Messages); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := adapter.ValidateToolDefs(req.Tools); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := adapter.ValidateFieldSizes(req.Messages); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := adapter.ValidateContentParts(req.Messages); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := adapter.ValidateResponseFormatSchema(req.ResponseFormat); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if req.Stream {
			handleStreamingChatCompletion(p, w, r, req)
			return
		}

		ctx := telemetry.ExtractContext(r.Context(), r)
		// Buffered path only -- see WithOverheadTracker's own doc comment
		// and docs/rfcs/2026-09-14-gateway-overhead-duration-header.md's
		// Detailed Design section for why the streaming path (below)
		// cannot set this same header: its Content-Type/Cache-Control/
		// Connection headers are already set before the pipeline even
		// runs, and there is no later point at which a header could still
		// be added to that same response.
		ctx, upstreamDuration := dataplane.WithOverheadTracker(ctx)
		requestStart := time.Now()
		resp, err := p.HandleChatCompletion(ctx, r.Header.Get("Authorization"), req, r.Header.Get("Idempotency-Key"))
		if err != nil {
			writeErrorResponse(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(overheadDurationHeader, strconv.FormatInt((time.Since(requestStart)-*upstreamDuration).Milliseconds(), 10))
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Error("encoding chat completion response", "error", err)
		}
	}
}

// embeddingsHandler adapts dataplane.Pipeline.HandleEmbeddings to
// net/http, mirroring chatCompletionsHandler's identical decode-run-encode
// shape. No streaming variant exists (embeddings has no streaming concept
// in any vendor's API), and no ValidateContentParts/ResponseFormatSchema
// calls (embeddings requests carry neither multi-modal content parts nor
// a response_format field) — otherwise the same request-size ceiling
// (maxRequestBodyBytes) and error-response convention as chat.
func embeddingsHandler(p *dataplane.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}

		var req adapter.EmbeddingRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}
		if req.Model == "" {
			http.Error(w, "model is required", http.StatusBadRequest)
			return
		}
		if len(req.Input) == 0 {
			http.Error(w, "input is required and must be non-empty", http.StatusBadRequest)
			return
		}

		ctx := telemetry.ExtractContext(r.Context(), r)
		resp, err := p.HandleEmbeddings(ctx, r.Header.Get("Authorization"), req)
		if err != nil {
			writeErrorResponse(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Error("encoding embeddings response", "error", err)
		}
	}
}

// handleStreamingChatCompletion runs the streaming pipeline and writes SSE
// chunks directly to w as they arrive. The response Content-Type is set
// before the pipeline runs, since it must be set before the first byte is
// written — but the actual HTTP status code is only implicitly finalized
// by the first real Write, exactly like the non-streaming path.
//
// Error handling is honest about a real limitation: writeErrorResponse
// works cleanly for every failure that happens BEFORE the first chunk is
// flushed (auth, rate-limit, unsupported-provider, not-configured, or an
// upstream connection that never sent a byte) — those produce a correct
// HTTP status code. A failure AFTER the first chunk has already reached
// the client cannot cleanly change the status code net/http already
// implied (200) when that first byte was flushed; writeErrorResponse's
// call still executes in that case (it does not crash), but only appends
// diagnostic text to an already-open SSE body rather than a clean status
// change — per docs/rfcs/2026-09-02-streaming-support.md's explicit
// acknowledgment that a mid-stream failure is a real, visible failure to
// the client, not smoothed over.
func handleStreamingChatCompletion(p *dataplane.Pipeline, w http.ResponseWriter, r *http.Request, req adapter.ChatRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := telemetry.ExtractContext(r.Context(), r)
	if err := p.HandleChatCompletionStream(ctx, r.Header.Get("Authorization"), req, w, r.Header.Get("Idempotency-Key")); err != nil {
		writeErrorResponse(w, err)
	}
}

// writeErrorResponse maps a HandleChatCompletion error to the appropriate
// HTTP status code.
func writeErrorResponse(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var capErr *dataplane.DeploymentCapacityError
	switch {
	case errors.Is(err, identity.ErrMissingHeader), errors.Is(err, identity.ErrInvalidKey):
		status = http.StatusUnauthorized
	case errors.Is(err, dataplane.ErrRateLimited), errors.Is(err, dataplane.ErrBudgetExceeded), errors.Is(err, dataplane.ErrConcurrencyLimitExceeded):
		// All three map to 429: OpenAI's own API returns 429 for both
		// literal rate-limit failures and budget/quota
		// ("insufficient_quota") failures, and Kelvran's canonical schema
		// explicitly targets OpenAI-SDK client compatibility — see
		// docs/rfcs/2026-09-02-virtual-keys-budgets.md's Alternatives
		// Considered section. ErrConcurrencyLimitExceeded joins the same
		// bucket per docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md
		// — from a client's perspective it's the same kind of "you're
		// being throttled" decision. All three are distinguished by the
		// error message body, not the status code.
		status = http.StatusTooManyRequests
	case errors.As(err, &capErr):
		// A deployment-capacity rejection is a backend-capacity condition
		// (503, per RFC 9110 §15.6.4/MDN's documented server-wants-to-
		// shed-load semantics), never a client-facing rate-limit decision
		// — see DeploymentCapacityError's own doc comment
		// (internal/gateway/dataplane/fallback.go) and
		// docs/upgrade-research/gateway-per-deployment-concurrency-
		// 2026-09-09.md. Deliberately its own case, never folded into the
		// 429 bucket above: the caller may be nowhere near its OWN rate
		// limit or concurrency cap.
		status = http.StatusServiceUnavailable
	case errors.Is(err, dataplane.ErrModelNotAllowed):
		status = http.StatusForbidden
	case errors.Is(err, dataplane.ErrStreamingNotSupported):
		status = http.StatusBadRequest
	case errors.Is(err, dataplane.ErrStreamingNotConfigured), errors.Is(err, dataplane.ErrEmbeddingsNotConfigured):
		status = http.StatusNotImplemented
	case errors.Is(err, dataplane.ErrNotAnEmbeddingDeployment):
		// 400, not the 502 default -- naming a model that resolves to a
		// chat (not embedding) deployment on the embeddings route is a
		// client request-shape mistake, never an upstream failure.
		status = http.StatusBadRequest
	case errors.Is(err, dataplane.ErrGuardrailBlocked):
		// 400, not the 502 default — a guardrail rejection is a
		// content-policy decision about THIS request, never an upstream
		// failure, matching OpenAI's own API convention for
		// moderation/content-policy rejections.
		status = http.StatusBadRequest
	case errors.Is(err, dataplane.ErrPromptAndMessagesBothSet), errors.Is(err, dataplane.ErrPromptResolutionFailed), errors.Is(err, dataplane.ErrPromptLabelAndVersionBothSet):
		// 400, the same "this request itself is malformed" bucket
		// ErrGuardrailBlocked already occupies — setting both prompt_id
		// and messages, both prompt_label and prompt_version, or naming
		// an unknown prompt_id/prompt_version/prompt_label, is a
		// client-request-shape problem, never an upstream failure.
		status = http.StatusBadRequest
	case errors.Is(err, dataplane.ErrResolvedPromptContentInvalid):
		// 400, the identical bucket -- a resolved prompt's own content
		// failing the MIME-spoof check is the same "this request itself
		// is malformed" shape as ErrPromptResolutionFailed, just caught
		// one step later (after resolution succeeded, not during it).
		status = http.StatusBadRequest
	}

	// Retry-After, per docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's
	// design (a) — set BEFORE http.Error below, since net/http requires
	// response headers to be set before WriteHeader (which http.Error
	// calls internally). A plain delay-seconds integer, per RFC 9110
	// §10.2.3's two allowed forms — the form every real client library,
	// including the OpenAI SDK's own retry logic, expects.
	var retryErr *dataplane.RetryAfterError
	if errors.As(err, &retryErr) {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(retryErr.RetryAfter)))
	}
	http.Error(w, err.Error(), status)
}

// retryAfterSeconds rounds d up to the nearest whole second, with a floor
// of 1 — Retry-After's integer form has no sub-second resolution, and a
// value of 0 would tell a client it may retry immediately, defeating the
// entire point of this header.
func retryAfterSeconds(d time.Duration) int {
	seconds := int((d + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}
