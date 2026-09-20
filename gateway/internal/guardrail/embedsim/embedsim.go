// Package embedsim implements an optional guardrail.Detector that flags
// text whose embedding is similar to a bundled corpus of known
// prompt-injection attacks and their paraphrases -- catches rewordings
// that promptinjection.go's own exact-phrase regex misses (e.g. "ignore
// all previous instructions" vs. "please disregard everything you were
// told before this message").
//
// This lives OUTSIDE internal/guardrail for the same reason
// bedrockguard does: that package's own doc comment states it "never
// imports adapter, cache, or any provider-specific package" -- a real
// embedding-model call is unavoidably provider-specific, so the
// implementation is a sibling package that merely satisfies
// guardrail.Detector.
//
// Deliberately never a replacement for promptinjection.go's regex
// detector, only an additional layer -- both run independently via
// Engine.Check, per guardrail.DefaultDetectors()'s own convention.
package embedsim

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	chromem "github.com/philippgille/chromem-go"

	"github.com/kelvran/gateway/gateway/internal/guardrail"
)

// Embedder produces a dense vector embedding for a piece of text.
// Injectable so tests can supply a deterministic fake instead of a real
// embedding-model call.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Config configures one Detector instance.
type Config struct {
	// SimilarityThreshold is the minimum cosine similarity (chromem-go's
	// Result.Similarity, range [-1,1]) against the closest known-attack
	// corpus entry required to report a Finding. Required, must be in
	// (0,1].
	SimilarityThreshold float64
	// CorpusPath, when non-empty, overrides the bundled default corpus
	// (corpus.json, embedded via go:embed) with a JSON file at this path
	// -- same shape, a top-level array of {"text": "..."} entries. Empty
	// uses the bundled default.
	CorpusPath string
}

//go:embed corpus.json
var defaultCorpusJSON []byte

type corpusEntry struct {
	Text string `json:"text"`
}

const collectionName = "known_prompt_injections"

// Detector implements guardrail.Detector.
type Detector struct {
	cfg        Config
	embedder   Embedder
	collection *chromem.Collection
}

// New constructs a Detector, eagerly seeding an in-memory vector
// collection from the corpus (cfg.CorpusPath, or the bundled default)
// using embedder -- one real embedding call per corpus entry, at
// construction time. This is a deliberate departure from
// bedrockguard.New's fully-lazy AWS-call convention: the corpus is
// small and fixed, so failing fast at startup (mirroring
// newBudgetTracker/boltstore.Open's own eager-fallible-construction
// pattern in cmd/gateway/main.go) is preferable to a confusing failure
// on an operator's first real request. An operator enabling this
// detector accepts a live embedding-model-reachability dependency at
// gateway startup as a result.
func New(cfg Config, embedder Embedder, logger *slog.Logger) (*Detector, error) {
	if cfg.SimilarityThreshold <= 0 || cfg.SimilarityThreshold > 1 {
		return nil, fmt.Errorf("embedsim: SimilarityThreshold must be in (0,1], got %v", cfg.SimilarityThreshold)
	}

	raw := defaultCorpusJSON
	if cfg.CorpusPath != "" {
		var err error
		raw, err = os.ReadFile(cfg.CorpusPath)
		if err != nil {
			return nil, fmt.Errorf("embedsim: reading corpus %q: %w", cfg.CorpusPath, err)
		}
	}
	var entries []corpusEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("embedsim: parsing corpus: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("embedsim: corpus has zero entries")
	}

	collection, err := chromem.NewDB().CreateCollection(collectionName, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("embedsim: creating vector collection: %w", err)
	}

	ctx := context.Background()
	ids := make([]string, len(entries))
	embeddings := make([][]float32, len(entries))
	contents := make([]string, len(entries))
	for i, entry := range entries {
		vec, err := embedder.Embed(ctx, entry.Text)
		if err != nil {
			return nil, fmt.Errorf("embedsim: embedding corpus entry %d: %w", i, err)
		}
		ids[i] = strconv.Itoa(i)
		embeddings[i] = vec
		contents[i] = entry.Text
	}
	if err := collection.Add(ctx, ids, embeddings, nil, contents); err != nil {
		return nil, fmt.Errorf("embedsim: seeding vector collection: %w", err)
	}

	logger.Info("embedsim_corpus_seeded", "entry_count", len(entries))
	return &Detector{cfg: cfg, embedder: embedder, collection: collection}, nil
}

func (d *Detector) Name() string { return "embedsim" }

// Category is CategoryPromptInjection unconditionally -- this Detector
// only ever augments promptinjection.go's own regex detector for the
// same threat class, matching bedrockguard.Detector's identical choice.
func (d *Detector) Category() guardrail.Category { return guardrail.CategoryPromptInjection }

// Detect implements guardrail.Detector.
func (d *Detector) Detect(ctx context.Context, text string) ([]guardrail.Finding, error) {
	vec, err := d.embedder.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("embedsim: embedding input: %w", err)
	}
	results, err := d.collection.QueryEmbedding(ctx, vec, 1, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("embedsim: querying vector collection: %w", err)
	}
	if len(results) == 0 || float64(results[0].Similarity) < d.cfg.SimilarityThreshold {
		return nil, nil
	}
	return []guardrail.Finding{{
		Category: guardrail.CategoryPromptInjection,
		Detector: "embedsim",
	}}, nil
}
