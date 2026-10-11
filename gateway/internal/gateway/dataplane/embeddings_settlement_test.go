package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// TestEmbeddingsSettleAnUncappedKeyLikeAChatCompletion: a key without a
// budget (BudgetUSD 0) is never reserved against -- budget.Tracker.Reserve
// short-circuits before any backend -- so its spend reaches the budget
// tracker only if settlement runs on the billable response itself. finalize
// does that for chat completions (a reservation OR a billable response
// settles); HandleEmbeddings settled only on a reservation, so an uncapped
// key's embeddings spend never reached GET /admin/virtual_keys/{name}/spend
// or `kelvran spend --by key` in any backend. The chat call is the positive
// control: the same key and tracker, spend recorded by the path that was
// already right.
func TestEmbeddingsSettleAnUncappedKeyLikeAChatCompletion(t *testing.T) {
	p, tracker := ledgerTestPipeline(t,
		func(_ context.Context, dep Deployment, _ any) (any, error) {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		},
		func(context.Context, Deployment, any) (any, error) {
			return &openai.EmbeddingResponseWire{
				Model: "text-embedding-3-small",
				Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1, 0.2}}},
				Usage: openai.EmbeddingUsageWire{PromptTokens: 20, TotalTokens: 20},
			}, nil
		}, spendledger.NewMemory(spendledger.Options{}))
	ctx := telemetry.WithAttribution(context.Background(), telemetry.Attribution{ClientTool: "curl"})

	if _, err := p.HandleChatCompletion(ctx, "Bearer other-key", "", "", ledgerChatReq, ""); err != nil {
		t.Fatal(err)
	}
	afterChat := tracker.SpentUSD(context.Background(), "other-key", 0)
	if afterChat.IsZero() {
		t.Fatal("control: a chat completion on the uncapped key recorded no spend; finalize no longer settles an unreserved billable response")
	}

	if _, err := p.HandleEmbeddings(ctx, "Bearer other-key", "", adapter.EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"hello"}}); err != nil {
		t.Fatal(err)
	}
	// The fixture prices text-embedding-3-small at 0.0001 USD per prompt
	// token; the fake answers with 20 of them.
	want := afterChat.Add(decimal.RequireFromString("0.002"))
	if got := tracker.SpentUSD(context.Background(), "other-key", 0); !got.Equal(want) {
		t.Fatalf("spend after an embeddings call on the uncapped key = %s, want %s (the chat spend plus 20 prompt tokens at 0.0001)", got, want)
	}
}

// TestEmbeddingsReleaseTheReservationWhenTheUpstreamFails: a capped key is
// reserved against before the upstream call (a cold key reserves its whole
// headroom), so when that call fails the reservation must be released, or the
// key stays debited by a request that cost nothing. This pins the
// `budgetReserved` half of the settlement gate; the uncapped-key test above
// pins the `realCost != nil` half. The failed call must leave the ledger
// empty too: nothing billable happened.
func TestEmbeddingsReleaseTheReservationWhenTheUpstreamFails(t *testing.T) {
	ledger := spendledger.NewMemory(spendledger.Options{})
	p, tracker := ledgerTestPipeline(t, nil, func(context.Context, Deployment, any) (any, error) {
		return nil, errors.New("upstream down")
	}, ledger)
	ctx := telemetry.WithAttribution(context.Background(), telemetry.Attribution{ClientTool: "curl"})
	if _, err := p.HandleEmbeddings(ctx, "Bearer test-key", "", adapter.EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"hello"}}); err == nil {
		t.Fatal("a failing upstream must surface as an error")
	}
	if got := tracker.SpentUSD(context.Background(), "test-key", 0); !got.IsZero() {
		t.Fatalf("spend after a failed embeddings call on the capped key = %s, want 0: the reservation was not released", got)
	}
	if rows := ledgerRows(t, ledger, spendledger.GroupByKey, ""); len(rows) != 0 {
		t.Fatalf("the ledger recorded %d row(s) for a failed embeddings call, want none", len(rows))
	}
}
