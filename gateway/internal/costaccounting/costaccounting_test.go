package costaccounting

import "testing"

func TestCalculateKnownModel(t *testing.T) {
	c := NewCalculator(PriceTable{
		"gpt-4o": {PromptPerToken: 0.000002, CompletionPerToken: 0.00001},
	})

	got := c.Calculate("gpt-4o", Usage{PromptTokens: 1000, CompletionTokens: 500})
	want := 1000*0.000002 + 500*0.00001
	if got != want {
		t.Errorf("Calculate() = %v, want %v", got, want)
	}
}

func TestCalculateUnknownModelReturnsZero(t *testing.T) {
	c := NewCalculator(PriceTable{
		"gpt-4o": {PromptPerToken: 0.000002, CompletionPerToken: 0.00001},
	})

	got := c.Calculate("some-unpriced-model", Usage{PromptTokens: 1000, CompletionTokens: 500})
	if got != 0 {
		t.Errorf("Calculate() for unknown model = %v, want 0", got)
	}
}

func TestCalculateZeroUsage(t *testing.T) {
	c := NewCalculator(PriceTable{
		"gpt-4o": {PromptPerToken: 0.000002, CompletionPerToken: 0.00001},
	})

	if got := c.Calculate("gpt-4o", Usage{}); got != 0 {
		t.Errorf("Calculate() with zero usage = %v, want 0", got)
	}
}
