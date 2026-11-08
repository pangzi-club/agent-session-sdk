package agentsession

import (
	"math"
	"regexp"
	"strings"
	"testing"
)

func strPtrTest(s string) *string { return &s }

func TestPricingTableShape(t *testing.T) {
	if got := len(modelPrices); got <= 100 {
		t.Fatalf("price table has %d models, want > 100", got)
	}
	for _, entry := range modelPrices {
		if entry.key != strings.ToLower(strings.TrimSpace(entry.key)) {
			t.Errorf("key %q is not normalized lowercase", entry.key)
		}
		// Normalization strips date and version suffixes; the table must not
		// contain them.
		if dateSuffixRe.MatchString(entry.key) || versionSuffixRe.MatchString(entry.key) {
			t.Errorf("key %q still carries a date/version suffix", entry.key)
		}
		for _, v := range entry.price {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
				t.Errorf("key %q has invalid rate %v", entry.key, v)
			}
		}
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(PricingSnapshot) {
		t.Errorf("snapshot %q is not YYYY-MM-DD", PricingSnapshot)
	}
	if PricingVersion == 0 {
		t.Error("PricingVersion must be nonzero")
	}
}

func TestPricingTableLockedRows(t *testing.T) {
	locks := map[string]ModelPrice{
		"gpt-5.6-sol":       {4, 20, 0.4, 5},
		"deepseek-flash":    {0.15, 0.6, 0.003, 0},
		"claude-sonnet-4-5": {3, 15, 0.3, 3.75},
		"grok-4.6":          {2, 6, 0.5, 0},
	}
	for key, want := range locks {
		if got := priceIndex[key]; got != want {
			t.Errorf("MODEL_PRICES[%q] = %v, want %v", key, got, want)
		}
	}
}

func TestPricingVersionMatchesTS(t *testing.T) {
	// Locked against the TS hash algorithm run over the same table (verified
	// via node + pricing.ts's hashPrices on the identical JSON): the JSON
	// transcription and the hash order must stay bit-identical across
	// languages. Bump this constant when model-prices.json changes, after
	// re-deriving it from the TS algorithm.
	const tsVersion = 64204264
	if PricingVersion != tsVersion {
		t.Errorf("PricingVersion = %d, want %d (TS)", PricingVersion, tsVersion)
	}
}

func TestNormalizeModelId(t *testing.T) {
	cases := map[string]string{
		"openai/gpt-5.6-sol":                           "gpt-5.6-sol",
		"openrouter/anthropic/claude-sonnet-4.5":       "claude-sonnet-4.5",
		"claude-sonnet-4-5-20250929":                   "claude-sonnet-4-5",
		"claude-sonnet-4-5@20250929":                   "claude-sonnet-4-5",
		"claude-sonnet-4-5-2025-09-29":                 "claude-sonnet-4-5",
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0": "claude-sonnet-4-5",
		"  GPT-5.6-SOL  ":                              "gpt-5.6-sol",
		"glm-5.3":                                      "glm-5.3",
	}
	for input, want := range cases {
		if got := NormalizeModelId(input); got != want {
			t.Errorf("NormalizeModelId(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLookupModelPrice(t *testing.T) {
	if got := LookupModelPrice(strPtrTest("openai/gpt-5.6-sol")); got == nil || *got != (ModelPrice{4, 20, 0.4, 5}) {
		t.Errorf("provider-prefixed lookup failed: %v", got)
	}
	if got := LookupModelPrice(strPtrTest("gpt-5.6-sol-2026-07-09")); got == nil || *got != (ModelPrice{4, 20, 0.4, 5}) {
		t.Errorf("date-suffixed lookup failed: %v", got)
	}
	// Copilot-style dotted spelling: retry dashed after an exact miss.
	if got := LookupModelPrice(strPtrTest("claude-opus-4.8")); got == nil || *got != (ModelPrice{5, 25, 0.5, 6.25}) {
		t.Errorf("dotted variant lookup failed: %v", got)
	}
	if got := LookupModelPrice(strPtrTest("grok-4.6-build")); got == nil || *got != (ModelPrice{2, 6, 0.5, 0}) {
		t.Errorf("alias lookup failed: %v", got)
	}
}

func TestLookupModelPriceUnknown(t *testing.T) {
	if got := LookupModelPrice(nil); got != nil {
		t.Errorf("null model should be unpriced, got %v", got)
	}
	if got := LookupModelPrice(strPtrTest("")); got != nil {
		t.Errorf("empty model should be unpriced, got %v", got)
	}
	if got := LookupModelPrice(strPtrTest("codex-auto-review")); got != nil {
		t.Errorf("internal codex name should be unpriced, got %v", got)
	}
	// No prefix matching: an unlisted pro variant never borrows its family's
	// price.
	if got := LookupModelPrice(strPtrTest("gpt-5.6-sol-pro")); got != nil {
		t.Errorf("unlisted variant must not guess, got %v", got)
	}
}

func TestEstimateTokenCost(t *testing.T) {
	usd, unpriced := EstimateTokenCost(TokenCostInput{
		Model: strPtrTest("gpt-5.6-sol"), InputTokens: 1_000_000,
	})
	if usd != 4 || unpriced != 0 {
		t.Errorf("input-only cost = (%v, %v), want (4, 0)", usd, unpriced)
	}

	// gpt-5.5: input 5 / output 30 / cacheRead 0.5 / cacheWrite 0 — a 0 cache
	// rate is "priced", not "free".
	usd, unpriced = EstimateTokenCost(TokenCostInput{
		Model: strPtrTest("gpt-5.5"), InputTokens: 1_000_000, OutputTokens: 1_000_000,
		CacheReadTokens: 1_000_000, CacheWriteTokens: 1_000_000,
	})
	if !nearlyEqual(usd, 35.5) || unpriced != 0 {
		t.Errorf("zero-cache-rate cost = (%v, %v), want (35.5, 0)", usd, unpriced)
	}

	usd, unpriced = EstimateTokenCost(TokenCostInput{
		Model: strPtrTest("codex-auto-review"), InputTokens: 10, OutputTokens: 5, CacheReadTokens: 1,
	})
	if usd != 0 || unpriced != 16 {
		t.Errorf("unknown model cost = (%v, %v), want (0, 16)", usd, unpriced)
	}
	usd, unpriced = EstimateTokenCost(TokenCostInput{Model: nil, InputTokens: 3})
	if usd != 0 || unpriced != 3 {
		t.Errorf("null model cost = (%v, %v), want (0, 3)", usd, unpriced)
	}
}

func TestEstimateTokenCostCredits(t *testing.T) {
	// CN Pro: ¥59 / 2000 credits ÷ 7.1 ≈ $0.0041549/credit.
	credits := 1000.0
	usd, unpriced := EstimateTokenCost(TokenCostInput{
		AgentID: "qoder-cn", Model: strPtrTest("qmodel_38max"), InputTokens: 10000,
		OutputTokens: 14, Credits: &credits,
	})
	if !nearlyEqual(usd, 1000*(59.0/2000.0/7.1)) || unpriced != 0 {
		t.Errorf("qoder-cn credit cost = (%v, %v)", usd, unpriced)
	}

	// Global: $0.01/credit; no model rates apply no matter the tokens.
	credits = 200
	usd, unpriced = EstimateTokenCost(TokenCostInput{
		AgentID: "qoder", Model: nil, InputTokens: 1_000_000, Credits: &credits,
	})
	if usd != 2 || unpriced != 0 {
		t.Errorf("qoder credit cost = (%v, %v), want (2, 0)", usd, unpriced)
	}

	// Missing credits (older transcripts): cost 0, nothing unpriced.
	usd, unpriced = EstimateTokenCost(TokenCostInput{
		AgentID: "qoder-cn", Model: strPtrTest("auto"), InputTokens: 12800, OutputTokens: 2,
	})
	if usd != 0 || unpriced != 0 {
		t.Errorf("missing credits cost = (%v, %v), want (0, 0)", usd, unpriced)
	}
}

func TestPricingCoversObservedModels(t *testing.T) {
	observed := []string{
		"gpt-5.6-sol", "gpt-5.5", "gpt-6-astra", "gpt-5.6-luna",
		"deepseek-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp",
		"deepseek-v4-pro", "deepseek-v4.1-flash", "grok-4.6", "grok-4.6-build",
	}
	for _, model := range observed {
		if got := LookupModelPrice(strPtrTest(model)); got == nil {
			t.Errorf("observed model %q should be priced", model)
		}
	}
}

func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}
