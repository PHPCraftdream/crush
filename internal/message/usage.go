package message

// CacheSupport records whether a message's cache counters carry information.
//
// Without it, zero is ambiguous: a provider that never reports caching and a
// genuine cache miss both store 0, and an analytics view would render the
// former as a confident "0% cache hit" — a lie. Every read path must branch on
// this rather than dividing blindly.
type CacheSupport string

const (
	// CacheSupportUnknown is the zero value: usage was recorded before this
	// field existed, or by a path that could not classify the provider.
	CacheSupportUnknown CacheSupport = ""
	// CacheSupportNative means the provider reported a real cache breakdown.
	// All four current providers (zai, claude-cli, codex-cli, gemini-cli) do.
	CacheSupportNative CacheSupport = "native"
	// CacheSupportNone means the provider is silent about caching, so the
	// cache counters mean "unknown", NOT "no hits".
	CacheSupportNone CacheSupport = "none"
)

// TokenUsage is the canonical, provider-independent token accounting for a
// single assistant message.
//
// CONVENTION (load-bearing — normalization happens at the provider boundary,
// never here): InputTokens counts ONLY tokens billed as fresh input. Tokens
// served from the prompt cache are in CacheReadTokens; tokens written into the
// cache are in CacheCreationTokens. The three are DISJOINT, so the prompt size
// is their sum.
//
// Providers disagree about this on the wire — claude reports an exclusive
// input count while codex and gemini fold the cache into theirs — which is
// exactly why internal/agent/cliprovider/usage.go exists. Anything reaching
// this type has already been normalized.
type TokenUsage struct {
	InputTokens         int64 `json:"input_tokens,omitempty"`
	OutputTokens        int64 `json:"output_tokens,omitempty"`
	ReasoningTokens     int64 `json:"reasoning_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`

	// TotalTokens is the provider's own figure when it published one, which
	// can EXCEED the itemized fields above: gemini counts thinking tokens its
	// stats block never breaks out. Do not assume it equals the sum.
	TotalTokens int64 `json:"total_tokens,omitempty"`

	// CostUSD is this message's own cost contribution, computed with the same
	// four-tier formula as the session total. Zero for flat-rate models and
	// for every local CLI model (they carry no prices at all).
	CostUSD float64 `json:"cost_usd,omitempty"`

	// Provider and Model record which model actually produced this message. A
	// session can switch models mid-conversation and a sub-agent can run a
	// different model than its parent, so the session's current selection is
	// not a safe substitute when aggregating.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	CacheSupport CacheSupport `json:"cache_support,omitempty"`

	// Estimated is true when the provider omitted usage entirely and the
	// numbers were synthesized from message lengths
	// (internal/agent/usage_fallback.go). Such rows are guesses and must be
	// flagged, not silently mixed into totals.
	Estimated bool `json:"estimated,omitempty"`
}

// PromptTokens is the full prompt size: fresh input plus both cache classes.
// This is the quantity that should be compared against a model's context
// window.
func (u TokenUsage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// CacheHitRatio returns the share of the prompt served from cache, in [0,1].
//
// ok is false when the number would be meaningless — the provider does not
// report caching, or there was no prompt to speak of. Callers MUST render
// "n/a" in that case instead of substituting zero; a fabricated 0% is
// indistinguishable from a real cache miss.
func (u TokenUsage) CacheHitRatio() (ratio float64, ok bool) {
	if u.CacheSupport != CacheSupportNative {
		return 0, false
	}
	prompt := u.PromptTokens()
	if prompt <= 0 {
		return 0, false
	}
	return float64(u.CacheReadTokens) / float64(prompt), true
}

// IsZero reports whether nothing was recorded. Used to avoid writing empty
// usage rows that would then be indistinguishable from a measured zero.
func (u TokenUsage) IsZero() bool {
	return u.InputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.ReasoningTokens == 0 &&
		u.CacheCreationTokens == 0 &&
		u.CacheReadTokens == 0 &&
		u.TotalTokens == 0 &&
		u.CostUSD == 0
}

// Add accumulates another message's usage into this one, for aggregates over a
// session or a group of sessions.
//
// Provider/Model are cleared when they disagree: a sum across two models
// belongs to neither. CacheSupport degrades to CacheSupportNone if ANY
// contributor lacked cache reporting, so a mixed aggregate cannot present a
// cache ratio computed from partial data. Estimated is sticky for the same
// reason — one guess makes the whole total a guess.
func (u TokenUsage) Add(other TokenUsage) TokenUsage {
	sum := TokenUsage{
		InputTokens:         u.InputTokens + other.InputTokens,
		OutputTokens:        u.OutputTokens + other.OutputTokens,
		ReasoningTokens:     u.ReasoningTokens + other.ReasoningTokens,
		CacheCreationTokens: u.CacheCreationTokens + other.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens + other.CacheReadTokens,
		TotalTokens:         u.TotalTokens + other.TotalTokens,
		CostUSD:             u.CostUSD + other.CostUSD,
		Estimated:           u.Estimated || other.Estimated,
	}

	switch {
	case u.IsZero() && u.Provider == "":
		// Adding onto an empty accumulator: adopt the incoming identity.
		sum.Provider, sum.Model = other.Provider, other.Model
		sum.CacheSupport = other.CacheSupport
	case u.Provider == other.Provider && u.Model == other.Model:
		sum.Provider, sum.Model = u.Provider, u.Model
		sum.CacheSupport = mergeCacheSupport(u.CacheSupport, other.CacheSupport)
	default:
		sum.CacheSupport = mergeCacheSupport(u.CacheSupport, other.CacheSupport)
	}
	return sum
}

// mergeCacheSupport takes the weakest of two classifications, so an aggregate
// never claims better cache visibility than its worst contributor.
func mergeCacheSupport(a, b CacheSupport) CacheSupport {
	if a == b {
		return a
	}
	if a == CacheSupportNone || b == CacheSupportNone {
		return CacheSupportNone
	}
	if a == CacheSupportUnknown || b == CacheSupportUnknown {
		return CacheSupportUnknown
	}
	return CacheSupportNative
}
