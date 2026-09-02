// Command gateway is Kelvran's single static binary: it loads the static
// YAML config, wires every internal component (identity, rate limiting,
// cache, provider adapters, cost accounting, the dataplane pipeline), and
// serves /v1/chat/completions in both buffered and streaming (SSE) modes.
//
// Streaming (real SSE, per docs/rfcs/2026-09-02-streaming-support.md) is
// wired for the OpenAI and Anthropic adapters only — a streaming request
// routed to Gemini/Bedrock/openaicompat returns a typed
// dataplane.ErrStreamingNotSupported (HTTP 400), never a silent fallback
// to buffering. Per docs/rfcs/2026-09-02-initial-code-scaffolding.md's
// scope boundary, this pass remains single-tenant with no guardrails/MCP/
// OTel wiring yet.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/kelvran/gateway/internal/adapter"
	"github.com/kelvran/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/internal/adapter/gemini"
	"github.com/kelvran/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/internal/adapter/openaicompat"
	"github.com/kelvran/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/internal/identity"
	"github.com/kelvran/gateway/internal/ratelimit"
)

// Rate-limit defaults for this pass. controlplane.Config carries no
// rate-limit knobs yet (not specified by this scaffolding pass's plan);
// these are conservative, documented placeholders, not a silent guess —
// tuning them per deployment is Phase 1 work alongside the Redis-backed
// distributed limiter.
const (
	defaultBurstCapacity   = 20
	defaultRefillPerSecond = 10
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the gateway's YAML config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(*configPath, logger); err != nil {
		logger.Error("gateway exited", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := controlplane.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		return fmt.Errorf("building pipeline: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))

	logger.Info("gateway listening", "addr", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, mux); err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// buildPipeline resolves every secret referenced by name in cfg from the
// environment and wires the full dataplane.Pipeline.
func buildPipeline(cfg *controlplane.Config, logger *slog.Logger) (*dataplane.Pipeline, error) {
	gatewayAPIKey := os.Getenv(cfg.APIKeyEnv)
	if gatewayAPIKey == "" {
		return nil, fmt.Errorf("environment variable %q (gateway API key) is not set", cfg.APIKeyEnv)
	}
	verifier, err := identity.NewVerifier(gatewayAPIKey)
	if err != nil {
		return nil, fmt.Errorf("constructing identity verifier: %w", err)
	}

	registry := adapter.Registry{
		"openai":       openai.New(),
		"anthropic":    anthropic.New(),
		"gemini":       gemini.New(),
		"bedrock":      bedrock.New(),
		"openaicompat": openaicompat.New(),
	}

	deployments := make([]dataplane.Deployment, 0, len(cfg.Deployments))
	for _, d := range cfg.Deployments {
		key := os.Getenv(d.APIKeyEnv)
		if key == "" {
			logger.Warn("deployment's upstream API key env var is not set; calls to this deployment will fail",
				"deployment", d.Name, "env_var", d.APIKeyEnv)
		}
		deployments = append(deployments, dataplane.Deployment{
			Name:          d.Name,
			Model:         d.Model,
			Provider:      d.Provider,
			UpstreamModel: d.UpstreamModel,
			BaseURL:       d.BaseURL,
			APIKey:        key,
		})
	}

	priceTable := costaccounting.PriceTable{}
	for model, price := range cfg.PriceTable {
		priceTable[model] = costaccounting.ModelPrice{
			PromptPerToken:     price.PromptPerToken,
			CompletionPerToken: price.CompletionPerToken,
		}
	}

	return dataplane.NewPipeline(dataplane.Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewTokenBucket(defaultBurstCapacity, defaultRefillPerSecond),
		Cache:          inprocess.New(),
		Adapters:       registry,
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(priceTable),
		Upstream:       dataplane.NewHTTPUpstreamCaller(&http.Client{Timeout: 60 * time.Second}),
		// Streaming upstream calls have no fixed response deadline — the
		// client.Timeout above would kill a long-running stream mid-way,
		// so streaming uses its own client with no overall timeout,
		// relying instead on the request's own context for cancellation.
		UpstreamStream: dataplane.NewHTTPUpstreamStreamCaller(&http.Client{}),
		Logger:         logger,
	})
}

// chatCompletionsHandler adapts dataplane.Pipeline.HandleChatCompletion to
// net/http: decode the canonical JSON request body, run the pipeline,
// encode the canonical JSON response (or an appropriate error status).
func chatCompletionsHandler(p *dataplane.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}

		var req adapter.ChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}

		if req.Stream {
			handleStreamingChatCompletion(p, w, r, req)
			return
		}

		resp, err := p.HandleChatCompletion(r.Context(), r.Header.Get("Authorization"), req)
		if err != nil {
			writeErrorResponse(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Error("encoding chat completion response", "error", err)
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

	if err := p.HandleChatCompletionStream(r.Context(), r.Header.Get("Authorization"), req, w); err != nil {
		writeErrorResponse(w, err)
	}
}

// writeErrorResponse maps a HandleChatCompletion error to the appropriate
// HTTP status code.
func writeErrorResponse(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, identity.ErrMissingHeader), errors.Is(err, identity.ErrInvalidKey):
		status = http.StatusUnauthorized
	case errors.Is(err, dataplane.ErrRateLimited):
		status = http.StatusTooManyRequests
	case errors.Is(err, dataplane.ErrStreamingNotSupported):
		status = http.StatusBadRequest
	case errors.Is(err, dataplane.ErrStreamingNotConfigured):
		status = http.StatusNotImplemented
	}
	http.Error(w, err.Error(), status)
}
