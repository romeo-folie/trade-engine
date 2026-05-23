package analysis

// ensemble.go — Phase 12.4: fast-path check, Opus arbiter, majority-vote fallback.

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/logger"
)

// ArbiterResult captures the full output of an arbiter synthesis call.
type ArbiterResult struct {
	Final          AnalysisResponse
	DissentSummary string
	ModelVotes     map[string]Action
	FastPathed     bool
	Meta           ModelCallMeta
}

// ─── Fast-path ─────────────────────────────────────────────────────────────────

// TryFastPath returns (signal, true) when all successful analysts unanimously
// agree and confidence metrics meet the thresholds; otherwise (_, false).
func TryFastPath(results []AnalystResult, minConf, maxSpread float64) (AnalysisResponse, bool) {
	successes := successfulResults(results)
	if len(successes) < 2 {
		return AnalysisResponse{}, false
	}
	action := successes[0].Response.Action
	for _, r := range successes[1:] {
		if r.Response.Action != action {
			return AnalysisResponse{}, false
		}
	}
	confs := make([]float64, len(successes))
	var sum float64
	for i, r := range successes {
		confs[i] = r.Response.Confidence
		sum += r.Response.Confidence
	}
	maxC, minC := confs[0], confs[0]
	for _, c := range confs[1:] {
		if c > maxC { maxC = c }
		if c < minC { minC = c }
	}
	if maxC < minConf || maxC-minC > maxSpread {
		logger.Debug("fast-path rejected", "maxC", maxC, "minC", minC, "spread", maxC-minC, "minConf", minConf, "maxSpread", maxSpread)
		return AnalysisResponse{}, false
	}
	avg := math.Round(sum/float64(len(confs))*1000) / 1000

	// Average SizePct, StopLoss, TakeProfit across all analysts so that risk
	// parameters are not determined by whichever analyst happened to respond fastest.
	var sumSizePct, sumSL, sumTP float64
	var countSL, countTP int
	for _, r := range successes {
		sumSizePct += r.Response.SizePct
		if r.Response.StopLoss > 0 {
			sumSL += r.Response.StopLoss
			countSL++
		}
		if r.Response.TakeProfit > 0 {
			sumTP += r.Response.TakeProfit
			countTP++
		}
	}
	n := float64(len(successes))
	avgSizePct := math.Round(sumSizePct/n*100) / 100
	var avgSL, avgTP float64
	if countSL > 0 {
		avgSL = math.Round(sumSL/float64(countSL)*10000) / 10000
	}
	if countTP > 0 {
		avgTP = math.Round(sumTP/float64(countTP)*10000) / 10000
	}

	return AnalysisResponse{
		Action:     action,
		Confidence: avg,
		SizePct:    avgSizePct,
		StopLoss:   avgSL,
		TakeProfit: avgTP,
		Reasoning:  "[fast-path] All analysts unanimous — confidence, size and risk levels averaged across analysts.",
	}, true
}

// ─── Arbiter ───────────────────────────────────────────────────────────────────

// Arbitrate sends all analyst results to the arbiter with anonymised labels
// (shuffled each call to prevent self-bias) and returns a synthesised signal.
func Arbitrate(ctx context.Context, arbiter ModelProvider, req AnalysisRequest, results []AnalystResult) (ArbiterResult, error) {
	if !arbiter.IsAvailable() {
		return ArbiterResult{}, fmt.Errorf("arbiter %s/%s is not available", arbiter.ProviderName(), arbiter.ModelName())
	}
	successes := successfulResults(results)
	if len(successes) == 0 {
		return ArbiterResult{}, fmt.Errorf("no successful analyst results to arbitrate")
	}
	votes := make(map[string]Action, len(successes))
	for _, r := range successes {
		votes[fmt.Sprintf("%s/%s", r.Provider, r.Model)] = r.Response.Action
	}

	// Shuffle to anonymise.
	indices := rand.Perm(len(successes))
	var sb strings.Builder
	for i, idx := range indices {
		r := successes[idx]
		// Generate enough unique labels for arbitrary ensemble sizes —
		// previously we cycled a fixed 5-label slice, so a 6-result combined
		// set (e.g. multi-timeframe re-arbitration with 3 analysts × 2
		// timeframes) re-used "Model-A" for two distinct results, making the
		// arbiter prompt ambiguous (Codex P2, Issue #321 PR #336).
		label := arbiterLabel(i)
		sb.WriteString(fmt.Sprintf("\n--- %s ---\nAction: %s\nConfidence: %.3f\nSizePct: %.1f%%\n",
			label, r.Response.Action, r.Response.Confidence, r.Response.SizePct))
		if r.Response.StopLoss > 0 {
			sb.WriteString(fmt.Sprintf("StopLoss: %.4f\n", r.Response.StopLoss))
		}
		if r.Response.TakeProfit > 0 {
			sb.WriteString(fmt.Sprintf("TakeProfit: %.4f\n", r.Response.TakeProfit))
		}
		sb.WriteString(fmt.Sprintf("Reasoning:\n%s\n", r.Response.Reasoning))
	}

	arbReq := AnalysisRequest{
		CustomSystemPrompt: arbiterSystemPrompt(),
		CustomUserMessage:  buildArbiterUserMessage(req.Ticker, sb.String()),
		Ticker:             req.Ticker,
	}

	t0 := time.Now()
	resp, meta, err := arbiter.Analyze(ctx, arbReq)
	if err != nil {
		return ArbiterResult{}, fmt.Errorf("arbiter call failed: %w", err)
	}
	meta.LatencyMs = time.Since(t0).Milliseconds()

	dissentSummary := extractDissent(resp.Reasoning)
	logger.Info("arbiter completed",
		"ticker", req.Ticker, "action", string(resp.Action),
		"confidence", fmt.Sprintf("%.3f", resp.Confidence),
		"dissent", dissentSummary != "", "latency_ms", meta.LatencyMs,
	)
	return ArbiterResult{Final: resp, DissentSummary: dissentSummary, ModelVotes: votes, FastPathed: false, Meta: meta}, nil
}

// arbiterLabel returns a unique anonymised label for the i-th analyst result
// in the arbiter prompt: Model-A..Model-Z, Model-AA..Model-ZZ, Model-AAA..,
// etc. True Excel-column-style encoder: unique for any i ≥ 0 with no
// collisions, including the 26→AA and 702→AAA roll-overs.
//
// Issue #321 PR #336: the round-2 fixup used `(i/26 - 1) % 26` which is
// modulo on the first character — `arbiterLabel(702)` would have wrapped
// back to "Model-AA" (same as i=26) instead of producing "Model-AAA".
// This implementation handles arbitrary i correctly.
func arbiterLabel(i int) string {
	s := ""
	i++ // bijective base-26 (Excel) is 1-indexed for the column scheme.
	for i > 0 {
		i--
		s = string(rune('A'+(i%26))) + s
		i /= 26
	}
	return "Model-" + s
}

// ─── Majority vote fallback ─────────────────────────────────────────────────────

// majorityVote returns the action with the most votes.
// Tie-breaking cascade: HOLD > directional > WAIT > SKIP.
// Rationale: HOLD means "no interest, drop from active consideration" — a real
// signal of disengagement, so it wins. Among directional (BUY/SELL) vs WAIT,
// directional wins: when analysts are split, refusing to act is a worse default
// than acting on the directional side they're proposing. WAIT is only the
// winner when it has an outright plurality. SKIP is ORB-mode's conservative
// gate and remains last. BUY-vs-SELL ties still resolve to HOLD (no directional
// bias on opposed signals).
func majorityVote(results []AnalystResult) (AnalysisResponse, bool) {
	successes := successfulResults(results)
	if len(successes) == 0 {
		return AnalysisResponse{}, false
	}
	counts := map[Action]int{}
	confSum := map[Action]float64{}
	for _, r := range successes {
		counts[r.Response.Action]++
		confSum[r.Response.Action] += r.Response.Confidence
	}
	// Find the maximum vote count dynamically over all observed actions so
	// non-standard actions (TRADE, SKIP) are counted when present.
	maxCount := 0
	for _, c := range counts {
		if c > maxCount {
			maxCount = c
		}
	}
	// Collect all actions tied for the lead.
	var leaders []Action
	for action, c := range counts {
		if c == maxCount {
			leaders = append(leaders, action)
		}
	}
	// Resolve ties. Two action vocabularies use this fallback and they never
	// mix in one ensemble: the stock pipeline emits BUY/SELL/HOLD/WAIT, the ORB
	// pipeline emits TRADE/SKIP. The only real-world tie pairs are therefore
	// WAIT-vs-directional (stock) and TRADE-vs-SKIP (ORB), so each is ordered
	// for its own context:
	//   - Stock: HOLD > BUY/SELL > WAIT. Production showed WAIT-wins-ties was
	//     silently downgrading directional signals; directional now wins so a
	//     1-BUY/1-WAIT (or 2/2) split produces a trade rather than a no-op.
	//   - ORB: SKIP > TRADE. SKIP stays ahead of TRADE so the conservative ORB
	//     gate is unchanged — a split ORB candidate must not become executable
	//     on the arbiter-fallback path (PR #346 review).
	// Opposed BUY-vs-SELL ties fall to HOLD (no directional bias).
	var winner Action
	if len(leaders) == 1 {
		winner = leaders[0]
	} else {
		hasBuy := counts[ActionBuy] == maxCount
		hasSell := counts[ActionSell] == maxCount
		hasTrade := counts[ActionTrade] == maxCount
		switch {
		case counts[ActionHold] == maxCount:
			winner = ActionHold // HOLD ("drop ticker") still wins ties — true disengagement signal.
		case hasBuy && hasSell:
			// Opposed directional signals — no directional bias, fall to HOLD.
			winner = ActionHold
		case hasBuy:
			winner = ActionBuy // directional beats WAIT on a tie.
		case hasSell:
			winner = ActionSell
		case counts[ActionSkip] == maxCount:
			winner = ActionSkip // SKIP beats TRADE on a tie (ORB conservative — unchanged).
		case hasTrade:
			winner = ActionTrade
		case counts[ActionWait] == maxCount:
			winner = ActionWait
		default:
			winner = ActionHold
		}
	}
	if maxCount == 0 {
		winner = ActionHold
	}
	var avgConf float64
	if counts[winner] > 0 {
		avgConf = math.Round(confSum[winner]/float64(counts[winner])*1000) / 1000
	}
	var ref AnalysisResponse
	for _, r := range successes {
		if r.Response.Action == winner {
			ref = r.Response
			break
		}
	}
	return AnalysisResponse{
		Action: winner, Confidence: avgConf,
		SizePct: ref.SizePct, StopLoss: ref.StopLoss, TakeProfit: ref.TakeProfit,
		Reasoning: fmt.Sprintf("[majority vote: %d/%d analysts agreed on %s]", maxCount, len(successes), winner),
	}, true
}

// ─── Prompt builders ───────────────────────────────────────────────────────────

func arbiterSystemPrompt() string {
	return `You are an expert quantitative trading arbiter. You receive market analyses from several independent AI analyst models (labelled Model-A, Model-B, etc.) for a single ticker. The labels are anonymised.

Your task:
1. Review each analysis for logical consistency and indicator interpretation accuracy.
2. Identify points of agreement and key disagreements.
3. Flag logical errors or unsupported claims.
4. Synthesise a final trading signal that best reflects the weight of evidence.
5. If analysts meaningfully disagree, begin your dissent note with "DISSENT:" in the reasoning field.

Output ONLY a valid JSON object:
{
  "action": "BUY" | "SELL" | "HOLD" | "WAIT",
  "confidence": <0.0–1.0>,
  "size_pct": <0.0–10.0>,
  "stop_loss": <price or 0>,
  "take_profit": <price or 0>,
  "reasoning": "<synthesis; include 'DISSENT: ...' if analysts disagreed>"
}

Action semantics:
- BUY / SELL: directional catalyst is present and the trade can be structured
  with a defined stop and target. You do not need perfect timing — markets
  rarely give it. A reasonable entry with a defined risk is sufficient.
- WAIT: use ONLY when you can name a specific, concrete blocker that prevents
  entry on this cycle. Acceptable blockers are factual: "price is %.2f above
  the named resistance at $X with no pullback printed", "earnings release in
  under 4 hours", "20%%+ gap with zero established intraday range and no
  volume confirmation". Vague language like "timing feels off", "wait for
  clarity", "let it settle", "consolidation in progress" is NOT a valid
  blocker — if you find yourself reaching for those phrases, return BUY or
  SELL with appropriately reduced confidence instead. Set size_pct to 0 for
  WAIT.
- HOLD: no meaningful catalyst or setup — nothing to track.

DECISION BIAS: when analysts disagree, default to the directional side they
propose unless WAIT is justified by a concrete, named blocker per the rules
above. Refusing to act is a decision with its own cost; treat it as one.

Do not include any text outside the JSON object.`
}

func buildArbiterUserMessage(ticker, analysesBlock string) string {
	return fmt.Sprintf("Ticker: %s\n\nThe following independent AI analyses have been submitted for arbitration:\n%s\n\nProduce your synthesised final signal as a JSON object.", ticker, analysesBlock)
}

func extractDissent(reasoning string) string {
	const marker = "DISSENT:"
	idx := strings.Index(strings.ToUpper(reasoning), marker)
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(reasoning[idx+len(marker):])
}

// ─── Helpers ───────────────────────────────────────────────────────────────────

func successfulResults(results []AnalystResult) []AnalystResult {
	out := make([]AnalystResult, 0, len(results))
	for _, r := range results {
		if r.Err == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LatencyMs < out[j].LatencyMs })
	return out
}
