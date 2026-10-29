package rudderstackai

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// TokenPrice holds token rates in USD per one million tokens.
type TokenPrice struct {
	// InputPerMtok prices uncached input tokens.
	InputPerMtok float64
	// OutputPerMtok prices output tokens.
	OutputPerMtok float64
	// CachedInputPerMtok prices cache-read input tokens. Nil: InputPerMtok.
	CachedInputPerMtok *float64
	// CacheWritePerMtok prices cache-write input tokens. Nil: InputPerMtok.
	CacheWritePerMtok *float64
	// CacheWrite1hPerMtok prices Anthropic 1-hour cache writes. Nil: CacheWritePerMtok.
	CacheWrite1hPerMtok *float64
}

// LongContextPrice holds the rates of every token of a call whose input exceeds ThresholdTokens.
type LongContextPrice struct {
	TokenPrice
	ThresholdTokens int
}

// ModelPrice is a model price in USD per one million tokens.
type ModelPrice struct {
	TokenPrice
	// Provider limits the price to calls from this provider (for example "anthropic"). Empty
	// matches any provider.
	Provider string
	// TierMultipliers is the token-cost multiplier per service tier, for example
	// {"batch": 0.5, "flex": 0.5, "fast": 2}. A tier without one is priced at 1.
	TierMultipliers map[string]float64
	// LongContext prices every token of a call whose input tokens exceed its threshold (for
	// example above 200K tokens). Its cache rates default to its own InputPerMtok. The tier
	// multiplier still applies.
	LongContext *LongContextPrice
	// ServerToolPerCall is USD per call of a provider-hosted tool, by kind: {"web_search": 0.01}.
	// Fees are added to a call's cost only when every kind the call used has one.
	ServerToolPerCall map[string]float64
}

type rates struct {
	input, output                        float64
	cachedInput, cacheWrite, cacheWrite1 *float64
}

type price struct {
	rates
	tiers       map[string]float64
	longContext *rates
	threshold   int
	serverTools map[string]float64
}

func validRate(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func tokenRates(p TokenPrice, prefix string) (rates, error) {
	if !validRate(p.InputPerMtok) || !validRate(p.OutputPerMtok) {
		return rates{}, fmt.Errorf("rudderstackai: RegisterPrice: %sInputPerMtok and %sOutputPerMtok must be finite numbers >= 0", prefix, prefix)
	}
	for name, v := range map[string]*float64{"CachedInputPerMtok": p.CachedInputPerMtok, "CacheWritePerMtok": p.CacheWritePerMtok, "CacheWrite1hPerMtok": p.CacheWrite1hPerMtok} {
		if v != nil && !validRate(*v) {
			return rates{}, fmt.Errorf("rudderstackai: RegisterPrice: %s%s must be a finite number >= 0", prefix, name)
		}
	}
	return rates{input: p.InputPerMtok, output: p.OutputPerMtok, cachedInput: p.CachedInputPerMtok, cacheWrite: p.CacheWritePerMtok, cacheWrite1: p.CacheWrite1hPerMtok}, nil
}

func priceEntry(model string, p ModelPrice) (string, price, error) {
	if model == "" {
		return "", price{}, errors.New("rudderstackai: RegisterPrice: model is required")
	}
	r, err := tokenRates(p.TokenPrice, "")
	if err != nil {
		return "", price{}, err
	}
	entry := price{rates: r}
	if p.TierMultipliers != nil {
		entry.tiers = map[string]float64{}
		for tier, v := range p.TierMultipliers {
			if !serviceTiers[tier] {
				return "", price{}, fmt.Errorf("rudderstackai: RegisterPrice: unknown service tier in TierMultipliers (%s)", tier)
			}
			if !validRate(v) {
				return "", price{}, fmt.Errorf("rudderstackai: RegisterPrice: TierMultipliers[%s] must be >= 0", tier)
			}
			entry.tiers[tier] = v
		}
	}
	if lc := p.LongContext; lc != nil {
		if lc.ThresholdTokens < 0 {
			return "", price{}, errors.New("rudderstackai: RegisterPrice: LongContext.ThresholdTokens must be >= 0")
		}
		lr, err := tokenRates(lc.TokenPrice, "LongContext.")
		if err != nil {
			return "", price{}, err
		}
		entry.longContext, entry.threshold = &lr, lc.ThresholdTokens
	}
	if p.ServerToolPerCall != nil {
		entry.serverTools = map[string]float64{}
		for kind, v := range p.ServerToolPerCall {
			if !validRate(v) {
				return "", price{}, fmt.Errorf("rudderstackai: RegisterPrice: ServerToolPerCall[%s] must be >= 0", kind)
			}
			entry.serverTools[kind] = v
		}
	}
	return strings.ToLower(p.Provider) + "|" + strings.ToLower(model), entry, nil
}

// RegisterPrice adds or replaces the price of a model. No prices ship with this package: list
// prices change and customers negotiate their own, so cost is only reported for models you price.
//
// model prices calls to exactly that model, or to that model with a version suffix: "gpt-4o" also
// prices "gpt-4o-2024-08-06" and a router's "openai/gpt-4o", never "gpt-4o-mini".
func RegisterPrice(model string, p ModelPrice) error {
	key, entry, err := priceEntry(model, p)
	if err != nil {
		return err
	}
	global.mu.Lock()
	global.prices[key] = entry
	global.mu.Unlock()
	return nil
}

// ClearPrices removes every registered price.
func ClearPrices() {
	global.mu.Lock()
	global.prices = map[string]price{}
	global.mu.Unlock()
}

// Labels naming the same service share prices; emitted labels stay as they are. The Gemini API
// ("gcp.gemini", formerly "google") and Vertex AI ("gcp.vertex_ai", formerly "google.vertex") are
// priced separately.
var sameService = map[string]string{"gcp.gemini": "google", "gcp.vertex_ai": "google.vertex"}

func service(provider string) string {
	if s, ok := sameService[provider]; ok {
		return s
	}
	return provider
}

// Bedrock cross-region inference profile prefixes (us.anthropic.claude-...).
var bedrockProfiles = []string{"us.", "us-gov.", "eu.", "apac.", "jp.", "au.", "in.", "global."}

// lookupPrice finds the registered price of a call: a registered name matches the model exactly
// or followed by a version suffix; the longest name wins, then a price for the call's provider,
// then one for its alias, then a generic one, then the more literal model form.
func lookupPrice(provider, model string) (price, bool) {
	if model == "" {
		return price{}, false
	}
	global.mu.RLock()
	defer global.mu.RUnlock()
	p := strings.ToLower(provider)
	m := strings.ToLower(model)
	if p == "azure.ai.openai" {
		if base, ok := global.cfg.azureDeployments[m]; ok {
			m = base
		}
	}
	candidates := []string{m}
	if p == "aws.bedrock" {
		for _, prefix := range bedrockProfiles {
			if strings.HasPrefix(m, prefix) {
				m = m[len(prefix):]
				candidates = append(candidates, m)
				break
			}
		}
		if dot := strings.Index(m, "."); dot >= 0 {
			candidates = append(candidates, m[dot+1:])
		}
	}
	if slash := strings.LastIndex(m, "/"); slash >= 0 && slash < len(m)-1 {
		candidates = append(candidates, m[slash+1:])
	}
	type match struct {
		length, rank, index int
		price               price
	}
	var best *match
	for key, pr := range global.prices {
		sep := strings.Index(key, "|")
		kp, km := key[:sep], key[sep+1:]
		rank := 0
		switch {
		case kp == p:
			rank = 2
		case kp != "" && service(kp) == service(p):
			rank = 1
		case kp != "":
			continue
		}
		for index, c := range candidates {
			if !isVersionOf(c, km) {
				continue
			}
			better := best == nil || len(km) > best.length ||
				(len(km) == best.length && (rank > best.rank || (rank == best.rank && index < best.index)))
			if better {
				best = &match{length: len(km), rank: rank, index: index, price: pr}
			}
		}
	}
	if best == nil {
		return price{}, false
	}
	return best.price, true
}

func round8(n float64) float64 { return math.Floor(n*1e8+0.5) / 1e8 }

func or(v *float64, fallback float64) float64 {
	if v != nil {
		return *v
	}
	return fallback
}

// priceCall returns the cost of a call and, separately, its server-tool fees (nil when it made
// no server-tool call or one of a kind without a fee); ok is false for an unpriced model.
func priceCall(provider, model string, input, output, cached, cacheWrite, cacheWrite1h int, tier string, serverTools map[string]int) (cost float64, serverToolCost *float64, ok bool) {
	pr, found := lookupPrice(provider, model)
	if !found {
		return 0, nil, false
	}
	r := pr.rates
	if pr.longContext != nil && input > pr.threshold {
		r = *pr.longContext
	}
	uncached := max(input-cached-cacheWrite, 0)
	write1h := min(cacheWrite1h, cacheWrite)
	writeRate := or(r.cacheWrite, r.input)
	tokens := (float64(uncached)*r.input +
		float64(cached)*or(r.cachedInput, r.input) +
		float64(cacheWrite-write1h)*writeRate +
		float64(write1h)*or(r.cacheWrite1, writeRate) +
		float64(output)*r.output) / 1_000_000
	multiplier := 1.0
	if m, has := pr.tiers[tier]; tier != "" && has {
		multiplier = m
	}
	used, all := 0, true
	fees := 0.0
	for kind, n := range serverTools {
		if n <= 0 {
			continue
		}
		used++
		fee, has := pr.serverTools[kind]
		if !has {
			all = false
			continue
		}
		fees += float64(n) * fee
	}
	if used > 0 && all {
		f := round8(fees)
		serverToolCost = &f
		return round8(tokens*multiplier + f), serverToolCost, true
	}
	return round8(tokens * multiplier), nil, true
}

// CostOptions is the usage detail [EstimateCost] prices beyond tokens.
type CostOptions struct {
	// CacheWrite1hTokens is the part of cacheWriteTokens written to Anthropic's 1-hour cache.
	CacheWrite1hTokens int
	// ServiceTier of the call, as the provider reports it or normalised (batch, flex, standard,
	// fast, other); applies the price's TierMultipliers.
	ServiceTier string
	// ServerToolCalls counts provider-hosted tool calls by kind ({"web_search": 2}), priced with
	// ServerToolPerCall when every kind used has a fee.
	ServerToolCalls map[string]int
}

// EstimateCost estimates the cost of a call in USD from the registered prices, as a recorded call
// is priced: long-context rates, the service tier's multiplier and server-tool fees apply.
// inputTokens includes cached and cache-write tokens. Only the first opts is used. ok is false
// for an unpriced model.
func EstimateCost(provider, model string, inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens int, opts ...CostOptions) (cost float64, ok bool) {
	var o CostOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	input, output, cached, write := nonNegative(inputTokens), nonNegative(outputTokens), nonNegative(cachedInputTokens), nonNegative(cacheWriteTokens)
	serverTools := map[string]int{}
	for kind, n := range o.ServerToolCalls {
		if n > 0 {
			serverTools[kind] = n
		}
	}
	c, _, ok := priceCall(provider, model, input, output, cached, write, min(nonNegative(o.CacheWrite1hTokens), write), normalizeServiceTier(provider, o.ServiceTier), serverTools)
	return c, ok
}

// ReportedCost reads a cost a provider or gateway reported: a number, or a decimal string
// ("0.0012", "1.2e-05"). Anything else (null, blank, negative) is no cost.
func ReportedCost(v any) (float64, bool) {
	var c float64
	switch t := v.(type) {
	case float64:
		c = t
	case int:
		c = float64(t)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		c = f
	default:
		return 0, false
	}
	if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		return 0, false
	}
	return c, true
}

// CreditCall is a successful call passed to the app's credit pricing hook after USD pricing.
type CreditCall struct {
	Provider, Model                     string
	InputTokens, OutputTokens           int
	ReasoningTokens                     *int
	CachedInputTokens, CacheWriteTokens int
	ServiceTier                         string
	CostUSD                             *float64
	CallCount                           int
	AgentName, CallPurpose              string
	FallbackUsed, BYOK                  *bool
}

// CreditsFunc returns app credits, or nil when the call has no credit price.
// Errors and panics are contained and reported as user_callback.
type CreditsFunc func(CreditCall) (*float64, error)

func creditsOf(v *float64) *float64 {
	if v != nil && validRate(*v) {
		return Ptr(*v)
	}
	return nil
}

func priceCredits(fn CreditsFunc, call CreditCall) (amount *float64) {
	defer func() {
		if r := recover(); r != nil {
			report(KindUserCallback, "credits", recovered(r))
			amount = nil
		}
	}()
	v, err := fn(call)
	if err != nil {
		report(KindUserCallback, "credits", err)
		return nil
	}
	return creditsOf(v)
}
