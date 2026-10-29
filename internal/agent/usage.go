package agent

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/beroni/bazel/internal/pricing"
)

// Usage is what an agent consumed from the model. It comes from the final
// stream-json event, which already sums the sub-agents it may have fired — an
// executable that does not speak that format returns everything zeroed, and
// the interface simply shows no spend at all.
//
// Model is the name of the main model that ran. It is what the price table
// uses to compute the cost: an unknown model falls back to tokens-only,
// without trying to guess a number that may mislead.
type Usage struct {
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CacheWrite   int     `json:"cache_write_tokens,omitempty"`
	CacheRead    int     `json:"cache_read_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	Model        string  `json:"model,omitempty"`
}

// Total is everything that went through the model. The cache counts: read or
// written, it's context the agent consumed to do the review.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheWrite + u.CacheRead
}

// Empty says there is no spend to show.
func (u Usage) Empty() bool { return u.Total() == 0 && u.CostUSD == 0 }

// add sums another spend into this one — it is how the steps of a pipeline
// become a single number at the end of the review. Model follows the sum as
// long as it comes from a single model: summing steps from different models
// zeroes Model, because no table row prices that mix.
func (u *Usage) add(o Usage) {
	switch {
	case u.Empty():
		u.Model = o.Model
	case o.Empty():
	case u.Model != o.Model:
		u.Model = ""
	}
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheWrite += o.CacheWrite
	u.CacheRead += o.CacheRead
	u.CostUSD += o.CostUSD
}

// Plus is the sum of two spends, without touching either one. Publishing a
// review after producing it is the same card spending twice, and the number it
// shows has to be the total of both.
func (u Usage) Plus(o Usage) Usage {
	u.add(o)
	return u
}

// String is the spend on one line, the way it appears at the end of the review.
func (u Usage) String() string {
	if u.Empty() {
		return ""
	}
	s := FormatTokens(u.Total()) + " tokens"
	if u.InputTokens+u.OutputTokens > 0 {
		s += fmt.Sprintf(" (in %s · out %s · cache %s)",
			FormatTokens(u.InputTokens), FormatTokens(u.OutputTokens),
			FormatTokens(u.CacheRead+u.CacheWrite))
	}
	if u.CostUSD > 0 {
		s += fmt.Sprintf(" · $%.2f", u.CostUSD)
	}
	return s
}

// StringWithCost is the spend in one line, making sure the cost shows up
// even if it did not come from the stream (using the pricing table). A cost
// that already came — from the provider or from the Runner, which already
// went through the table — is not redone: here we only fill in what was
// missing.
func (u Usage) StringWithCost(pricing *pricing.Table) string {
	if u.Empty() {
		return ""
	}
	if pricing != nil && u.CostUSD == 0 {
		u.CostUSD = pricing.Cost(u.Model, u.InputTokens, u.OutputTokens, u.CacheWrite, u.CacheRead)
	}
	s := FormatTokens(u.Total()) + " tokens"
	if u.InputTokens+u.OutputTokens > 0 {
		s += fmt.Sprintf(" (in %s · out %s · cache %s)",
			FormatTokens(u.InputTokens), FormatTokens(u.OutputTokens),
			FormatTokens(u.CacheRead+u.CacheWrite))
	}
	if u.CostUSD > 0 {
		s += fmt.Sprintf(" · $%.2f", u.CostUSD)
	}
	return s
}

// priced redoes the cost from the price table. byModel is the spend split
// by model, when the adapter has it: a Claude Code run mixes models — the
// main one, the lenses that ran as a sub-agent, the auxiliary ones — and
// each one is charged its own price. A model the table does not know keeps
// the cost the provider itself reported for it. With no known model at all,
// the provider's cost stays as it came — zeroing it would erase the only
// number that exists.
func priced(u Usage, byModel []Usage, table *pricing.Table) Usage {
	if table == nil {
		return u
	}
	parts := byModel
	if len(parts) == 0 {
		parts = []Usage{u}
	}
	var (
		total float64
		known bool
	)
	for _, p := range parts {
		if c, ok := table.CostOf(p.Model, p.InputTokens, p.OutputTokens, p.CacheWrite, p.CacheRead); ok {
			total += c
			known = true
		} else {
			total += p.CostUSD
		}
	}
	// Not rounded here: a review sums its steps, and rounding each step would
	// zero out the cheap ones. String and the page round when displaying.
	if known {
		u.CostUSD = total
	}
	return u
}

// FormatTokens abbreviates the count: a fleet review burns millions of
// tokens, and "1,8M" reads better than the seven digits.
func FormatTokens(n int) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return trimZero(float64(n)/1000) + "k"
	default:
		return trimZero(float64(n)/1_000_000) + "M"
	}
}

// trimZero rounds to one decimal place, with a comma, and drops the ",0".
func trimZero(v float64) string {
	s := strings.Replace(strconv.FormatFloat(v, 'f', 1, 64), ".", ",", 1)
	return strings.TrimSuffix(s, ",0")
}
