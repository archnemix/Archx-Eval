// Package draw turns a validated draw request into a concrete set of examples.
//
// Two rules govern selection:
//  1. A caller may never receive more than DrawMax examples in one draw.
//  2. Each category gets its share of the budget proportionally; any leftover
//     budget is re-distributed across categories that still have stock. This
//     keeps a single scarce category from starving the others.
//
// Every example is handed out at most once per request and is drawn uniformly
// at random from the still-usable pool, so repeated draws do not follow a
// predictable order.
package draw

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
	"github.com/archnemix/archx-eval/relay/internal/bank"
)

// Request is a validated draw instruction.
type Request struct {
	Categories []string
	// Mix optionally pins the split, e.g. map[python]0.6. Empty means
	// "proportional to available stock".
	Mix  map[string]float64
	Size int
}

// Result is the outcome of a draw.
type Result struct {
	Examples  []*bank.Example
	ByCat     map[string]int
	Shortfall int
	Notes     []string
}

// Select performs the draw, returning at most req.Size examples.
func Select(m *bank.Manager, req Request) (*Result, error) {
	if req.Size <= 0 {
		return nil, apierr.BadRequest("size must be greater than zero")
	}
	if len(req.Categories) == 0 {
		return nil, apierr.BadRequest("at least one category is required")
	}

	cats, err := resolveCategories(m, req.Categories)
	if err != nil {
		return nil, err
	}

	// Collect eligible stock per category across all difficulties.
	pools := map[string][]*bank.Example{}
	for _, cat := range cats {
		for _, diff := range bank.Difficulties {
			pools[cat] = append(pools[cat], m.Eligible(cat, diff)...)
		}
	}

	res := &Result{ByCat: map[string]int{}}
	totalStock := 0
	for _, cat := range cats {
		totalStock += len(pools[cat])
	}
	if totalStock == 0 {
		return nil, apierr.Unavailable("the bank has no usable examples left for the requested categories")
	}

	// Budget per category, proportional by default.
	desired := map[string]int{}
	remaining := req.Size
	if len(req.Mix) > 0 {
		weightSum := 0.0
		for _, cat := range cats {
			w, ok := req.Mix[cat]
			if !ok {
				continue
			}
			weightSum += w
		}
		if weightSum <= 0 {
			return nil, apierr.BadRequest("mix weights must sum to a positive value")
		}
		for _, cat := range cats {
			w := req.Mix[cat]
			share := int(float64(req.Size) * (w / weightSum))
			desired[cat] = share
			remaining -= share
		}
	} else {
		for _, cat := range cats {
			share := req.Size * len(pools[cat]) / totalStock
			desired[cat] = share
			remaining -= share
		}
	}

	// Shuffle each category once and draw from the front of that deck. The
	// earlier code sampled the proportional share at random and then sliced
	// pools[cat][taken:] for the remainder, so the second pass could hand out
	// an example the first pass had already picked. A grant with the same
	// question twice is unusable: the reporter sends two answers for one id and
	// the feedback endpoint refuses the whole report as a duplicate.
	decks := make(map[string][]*bank.Example, len(cats))
	for _, cat := range cats {
		decks[cat] = shuffledCopy(pools[cat])
	}

	// Hand out the proportional share first.
	taken := map[string]int{}
	for _, cat := range cats {
		n := desired[cat]
		if n > len(decks[cat]) {
			n = len(decks[cat])
		}
		if n <= 0 {
			continue
		}
		picked := decks[cat][:n]
		res.Examples = append(res.Examples, picked...)
		taken[cat] = n
		remaining -= n - desired[cat]
	}

	// Distribute the remainder to any category that still has stock.
	for remaining > 0 {
		progressed := false
		for _, cat := range cats {
			if remaining <= 0 {
				break
			}
			// Both passes draw from the same shuffled deck and the index only
			// moves forward, so an example can never be issued twice.
			if taken[cat] >= len(decks[cat]) {
				continue
			}
			res.Examples = append(res.Examples, decks[cat][taken[cat]])
			taken[cat]++
			remaining--
			progressed = true
		}
		if !progressed {
			break
		}
	}

	// Shortfall is derived once, from the truth: what we actually issued.
	if issued := len(res.Examples); issued < req.Size {
		res.Shortfall = req.Size - issued
		res.Notes = append(res.Notes, fmt.Sprintf("served %d of %d requested; limited stock", issued, req.Size))
	}

	for _, ex := range res.Examples {
		res.ByCat[ex.Category]++
	}
	shuffleExamples(res.Examples)
	return res, nil
}

func resolveCategories(m *bank.Manager, requested []string) ([]string, error) {
	available := map[string]bool{}
	for _, c := range m.Categories() {
		available[c] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range requested {
		cat := strings.ToLower(strings.TrimSpace(raw))
		if cat == "" || seen[cat] {
			continue
		}
		if !available[cat] {
			return nil, apierr.BadRequest("unknown category: " + cat)
		}
		seen[cat] = true
		out = append(out, cat)
	}
	if len(out) == 0 {
		return nil, apierr.BadRequest("no usable category supplied")
	}
	sort.Strings(out)
	return out, nil
}

// sample returns n distinct elements chosen uniformly at random.
func sample(pool []*bank.Example, n int) []*bank.Example {
	if n <= 0 || len(pool) == 0 {
		return nil
	}
	if n >= len(pool) {
		out := make([]*bank.Example, len(pool))
		copy(out, pool)
		return out
	}
	// Partial Fisher-Yates: only shuffle the prefix we need.
	tmp := make([]*bank.Example, len(pool))
	copy(tmp, pool)
	for i := 0; i < n; i++ {
		j := i + randIndex(len(tmp)-i)
		tmp[i], tmp[j] = tmp[j], tmp[i]
	}
	return tmp[:n]
}

// shuffledCopy returns a randomly ordered copy; the pool itself is not touched.
func shuffledCopy(pool []*bank.Example) []*bank.Example {
	out := make([]*bank.Example, len(pool))
	copy(out, pool)
	shuffleExamples(out)
	return out
}

func shuffleExamples(list []*bank.Example) {
	for i := len(list) - 1; i > 0; i-- {
		j := randIndex(i + 1)
		list[i], list[j] = list[j], list[i]
	}
}

// randIndex returns a cryptographically random int in [0,n) rather than a
// predictable pseudo-random sequence, so a caller cannot infer draw order.
func randIndex(n int) int {
	if n <= 1 {
		return 0
	}
	bigN, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(bigN.Int64())
}

// Category shares helper for validation of a mix map.
func ValidateMix(mix map[string]float64) error {
	for cat, w := range mix {
		if w < 0 {
			return errors.New("mix weight for " + cat + " is negative")
		}
	}
	return nil
}
