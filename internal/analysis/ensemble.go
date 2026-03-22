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
	labels := []string{"Model-A", "Model-B", "Model-C", "Model-D", "Model-E"}
	var sb strings.Builder
	for i, idx := range indices {
		r := successes[idx]
		label := labels[i%len(labels)]
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

// ─── Majority vote fallback ─────────────────────────────────────────────────────

// majorityVote returns the action with the most votes; HOLD wins ties.
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
	// Find the maximum vote count across all actions.
	maxCount := 0
	for _, action := range []Action{ActionBuy, ActionSell, ActionHold} {
		if counts[action] > maxCount {
			maxCount = counts[action]
		}
	}
	// Collect all actions that share the maximum vote count.
	var leaders []Action
	for _, action := range []Action{ActionBuy, ActionSell, ActionHold} {
		if counts[action] == maxCount {
			leaders = append(leaders, action)
		}
	}
	// If BUY and SELL are both tied for the lead, the directional signal is
	// ambiguous — return HOLD (neutral) rather than favouring BUY by iteration order.
	var winner Action
	if len(leaders) == 1 {
		winner = leaders[0]
	} else {
		// Multiple actions tied: always prefer HOLD over a directional bias.
		winner = ActionHold
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
  "action": "BUY" | "SELL" | "HOLD",
  "confidence": <0.0–1.0>,
  "size_pct": <0.0–10.0>,
  "stop_loss": <price or 0>,
  "take_profit": <price or 0>,
  "reasoning": "<synthesis; include 'DISSENT: ...' if analysts disagreed>"
}

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
