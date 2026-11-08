package agentsession

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Pricing estimates USD cost from token counts. Pure functions, no IO; it is
// an estimate, not a bill (gateway discounts, subscription tiers, long-context
// bands and reasoning splits are not reflected).
//
// Known conventions:
//   - cost = Σ tokens × table rate / 1M;
//   - Qoder agents bill by credits (tokens are heuristic reconstructions), so
//     their cost = credits × per-credit price; free requests (billable=false)
//     price their credits too;
//   - a model missing from the table is never guessed: its tokens count as
//     unpricedTokens and callers show the cost as a lower bound;
//   - a 0 cache rate in the table means "not billed separately", not "free";
//   - zcode input can be negative (the scan layer subtracts without clamping)
//     and participates in the arithmetic as-is.

//go:embed model-prices.json
var modelPricesRaw []byte

// PricingSnapshot is the models.dev snapshot date the table was taken from.
var PricingSnapshot string

// ModelPrice is the per-1M-token rate tuple [input, output, cacheRead,
// cacheWrite] in USD.
type ModelPrice = [4]float64

// modelAliases maps table-missing names onto models known to share a price
// (both sides normalized, lowercase). Only entries with a clear provenance.
var modelAliases = map[string]string{
	// xAI CLI build edition, priced like grok-4.6 in the xai directory
	"grok-4.6-build": "grok-4.6",
}

// creditPriceUSD converts one credit to USD for credit-billed agents
// (subscription-tier average, an estimate not a bill):
// qoder-cn: CN Pro ¥59 / 2000 credits ≈ ¥0.0295/credit at 7.1 CNY/USD;
// qoder: global edition ≈ $10 / 1000 credits.
var creditPriceUSD = map[string]float64{
	"qoder-cn": 59.0 / 2000.0 / 7.1,
	"qoder":    0.01,
}

// modelPrices holds the table in file (declaration) order so that
// PricingVersion hashes identically to the TS implementation.
var modelPrices []modelPriceEntry

type modelPriceEntry struct {
	key   string
	price ModelPrice
}

func init() {
	var doc struct {
		Snapshot string                `json:"snapshot"`
		Order    []string              `json:"order"`
		Prices   map[string]ModelPrice `json:"prices"`
	}
	if err := json.Unmarshal(modelPricesRaw, &doc); err != nil {
		panic("agentsession: embedded model-prices.json is invalid: " + err.Error())
	}
	PricingSnapshot = doc.Snapshot
	priceIndex = make(map[string]ModelPrice, len(doc.Order))
	modelPrices = make([]modelPriceEntry, 0, len(doc.Order))
	for _, key := range doc.Order {
		price := doc.Prices[key]
		priceIndex[key] = price
		modelPrices = append(modelPrices, modelPriceEntry{key: key, price: price})
	}
	PricingVersion = computePricingVersion()
}

var (
	providerPrefixRe = regexp.MustCompile(`^(?:[a-z0-9._-]+/)+`)
	vendorPrefixRe   = regexp.MustCompile(`^(?:[a-z0-9-]+\.)*(?:anthropic|openai|google|meta|xai|amazon|mistral|deepseek|moonshot|zhipu|minimax|alibaba)\.`)
	versionSuffixRe  = regexp.MustCompile(`-v\d+(?::\d+)?$`)
	dateSuffixRe     = regexp.MustCompile(`(?:@|-)(?:\d{8}|\d{4}-\d{2}-\d{2})$`)
	revisionSuffixRe = regexp.MustCompile(`:\d+$`)
)

// NormalizeModelId normalizes a model name: lowercase, strip provider and
// dotted-vendor prefixes, strip trailing version and date suffixes. Examples:
// `openai/gpt-5.6-sol` → `gpt-5.6-sol`; `claude-sonnet-4-5@20250929` →
// `claude-sonnet-4-5`; `us.anthropic.claude-sonnet-4-5-20250929-v1:0` →
// `claude-sonnet-4-5`.
func NormalizeModelId(model string) string {
	s := strings.TrimSpace(model)
	s = strings.ToLower(s)
	s = providerPrefixRe.ReplaceAllString(s, "")
	s = vendorPrefixRe.ReplaceAllString(s, "")
	s = versionSuffixRe.ReplaceAllString(s, "")
	s = revisionSuffixRe.ReplaceAllString(s, "")
	s = dateSuffixRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

type lookupResult struct {
	price ModelPrice
	found bool
}

var lookupCache sync.Map // normalized name -> lookupResult (negatives cached too)

// LookupModelPrice resolves the price tuple for a model: exact normalized
// hit → dotted-name variant (`claude-opus-4.8` ↔ `claude-opus-4-8`) → alias
// table. No prefix matching: an unlisted variant like `gpt-5.6-sol-pro` is
// unpriced rather than priced like its family.
func LookupModelPrice(model *string) *ModelPrice {
	if model == nil {
		return nil
	}
	key := NormalizeModelId(*model)
	if key == "" {
		return nil
	}
	if cached, ok := lookupCache.Load(key); ok {
		result := cached.(lookupResult)
		if !result.found {
			return nil
		}
		price := result.price
		return &price
	}
	price := resolvePrice(key)
	var cached lookupResult
	if price != nil {
		cached = lookupResult{price: *price, found: true}
	}
	lookupCache.Store(key, cached)
	if price == nil {
		return nil
	}
	return price
}

func resolvePrice(key string) *ModelPrice {
	if price, ok := priceByKey(key); ok {
		return &price
	}
	dashed := strings.ReplaceAll(key, ".", "-")
	if dashed != key {
		if price, ok := priceByKey(dashed); ok {
			return &price
		}
	}
	alias, ok := modelAliases[key]
	if !ok {
		alias, ok = modelAliases[dashed]
	}
	if !ok {
		return nil
	}
	return resolvePrice(alias)
}

var priceIndex map[string]ModelPrice

func priceByKey(key string) (ModelPrice, bool) {
	price, ok := priceIndex[key]
	return price, ok
}

// TokenCostInput is one usage record prepared for pricing. AgentId routes
// credit-billed agents (qoder / qoder-cn) away from model rates.
type TokenCostInput struct {
	AgentID          string
	Model            *string
	InputTokens      float64
	OutputTokens     float64
	CacheReadTokens  float64
	CacheWriteTokens float64
	Credits          *float64
}

// EstimateTokenCost prices one usage record. Credit-billed agents pay
// credits × per-credit price (missing credits — older transcripts — cost 0
// and count nothing as unpriced). Everyone else pays model rates; with no
// resolvable price (model null included) usd is 0 and every token of the
// record counts as unpriced (the four-way sum, matching the report totals).
func EstimateTokenCost(usage TokenCostInput) (usd, unpricedTokens float64) {
	if price, ok := creditPriceUSD[usage.AgentID]; ok {
		credits := 0.0
		if usage.Credits != nil {
			credits = *usage.Credits
		}
		return credits * price, 0
	}
	totalTokens := usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	price := LookupModelPrice(usage.Model)
	if price == nil {
		return 0, totalTokens
	}
	usd = (usage.InputTokens*price[0] +
		usage.OutputTokens*price[1] +
		usage.CacheReadTokens*price[2] +
		usage.CacheWriteTokens*price[3]) / 1_000_000
	return usd, 0
}

// PricingVersion is a content hash (FNV-1a 32-bit, decimal) of the price
// table plus the credit prices: it changes whenever the table does, so caches
// that embed it invalidate themselves without a manual version bump. It
// equals the TS PRICING_VERSION for the same table. Assigned at the end of
// init so the table is loaded first (package-level var initializers run
// before init functions).
var PricingVersion uint32

func computePricingVersion() uint32 {
	hash := hashPriceTable(modelPrices)
	creditEntries := []modelPriceEntry{
		// Deterministic order: the TS table literal lists qoder-cn first.
		{key: "qoder-cn", price: ModelPrice{creditPriceUSD["qoder-cn"]}},
		{key: "qoder", price: ModelPrice{creditPriceUSD["qoder"]}},
	}
	return hash ^ hashCreditEntries(creditEntries)
}

const fnvOffset32 = 2166136261

// hashPriceTable mixes each key and its rate tuple into the hash in table
// order (JSON order here, declaration order in TS — identical by
// construction).
func hashPriceTable(entries []modelPriceEntry) uint32 {
	hash := uint32(fnvOffset32)
	for _, entry := range entries {
		hash = fnvMixString(hash, entry.key)
		parts := make([]string, 4)
		for i, v := range entry.price {
			parts[i] = formatPriceNumber(v)
		}
		hash = fnvMixString(hash, strings.Join(parts, ","))
	}
	return hash
}

func hashCreditEntries(entries []modelPriceEntry) uint32 {
	hash := uint32(fnvOffset32)
	for _, entry := range entries {
		hash = fnvMixString(hash, entry.key)
		hash = fnvMixString(hash, formatPriceNumber(entry.price[0]))
	}
	return hash
}

func fnvMixString(hash uint32, text string) uint32 {
	for _, r := range []byte(text) {
		hash ^= uint32(r)
		hash *= 16777619
	}
	return hash
}

// formatPriceNumber renders a number the way JavaScript's String(number) does
// for the values this table contains (integer or minimal decimal form).
func formatPriceNumber(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
