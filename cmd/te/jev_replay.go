package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
	"github.com/romeo-folie/trade-engine/internal/analysis/snapshot"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/jev"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
)

// Each output line retains both paid calls (including failures), enabling
// inspection without modifying immutable engine decision records.
type jevReplayResult struct {
	SnapshotID     string                     `json:"snapshot_id"`
	Ticker         string                     `json:"ticker"`
	Variant        string                     `json:"variant"`
	RecordedModel  string                     `json:"recorded_model"`
	RecordedOutput json.RawMessage            `json:"recorded_output"`
	Meta           analysis.ModelCallMeta     `json:"meta"`
	Answers        map[string]jev.Answer      `json:"answers,omitempty"`
	Output         *analysis.AnalysisResponse `json:"output,omitempty"`
	Trace          *jev.Trace                 `json:"trace,omitempty"`
	Error          string                     `json:"error,omitempty"`
}

var jevReplayCmd = &cobra.Command{
	Use:   "jev-replay <snapshot-id> [snapshot-id...]",
	Short: "Compare recorded JEV questions with experimental entry evidence (paid calls; no orders)",
	Long:  "Makes two fresh model calls per frozen snapshot: recorded baseline and entry-evidence experiment. Uses recorded policy thresholds, never fetches market data or executes trades, and writes JSONL to a new output file. Model aliases may resolve differently from historical calls; each call records its served version. This is an input experiment, not a trading backtest.",
	Args:  cobra.RangeArgs(1, 10),
	RunE: func(cmd *cobra.Command, args []string) error {
		output, _ := cmd.Flags().GetString("output")
		if output == "" {
			return fmt.Errorf("--output is required")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		model, _ := cmd.Flags().GetString("model")
		if model == "" {
			model = cfg.Analysis.Jev.Model
		}
		client, err := jev.NewClient(jev.ClientConfig{APIKey: cfg.Analysis.Jev.APIKey, BaseURL: cfg.Analysis.Jev.BaseURL, Model: model, Timeout: time.Duration(cfg.Analysis.Jev.TimeoutSec) * time.Second, MaxRetries: 0})
		if err != nil {
			return err
		}
		if err := connectDB(); err != nil {
			return err
		}
		defer store.Close()
		f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		failed := false
		for _, id := range args {
			s, err := store.GetDecisionSnapshot(cmd.Context(), id)
			if err != nil {
				return err
			}
			if s == nil {
				return fmt.Errorf("snapshot %s not found", id)
			}
			calls, err := store.GetDecisionStageCalls(cmd.Context(), id)
			if err != nil {
				return err
			}
			var recorded *store.DecisionStageCall
			for i := range calls {
				if calls[i].Stage == "jev" && calls[i].Error == "" && calls[i].RawPrompt != "" {
					recorded = &calls[i]
				}
			}
			if recorded == nil {
				return fmt.Errorf("snapshot %s has no successful JEV call", id)
			}
			var in snapshot.Inputs
			if err := json.Unmarshal(s.Inputs, &in); err != nil {
				return err
			}
			var effective snapshot.EffectiveConfig
			if err := json.Unmarshal(s.EffectiveConfig, &effective); err != nil {
				return err
			}
			if effective.JevPolicy == nil {
				return fmt.Errorf("snapshot %s lacks recorded JEV policy", id)
			}
			policyJSON, err := json.Marshal(effective.JevPolicy)
			if err != nil {
				return err
			}
			var policy jev.PolicyConfig
			if err := json.Unmarshal(policyJSON, &policy); err != nil {
				return err
			}
			opts := jev.Options{Shorts: effective.Shorts != nil && effective.Shorts.Enabled}
			questionVersion, policyVersion := opts.Versions()
			if s.SchemaVersion != snapshot.SchemaVersion || s.PolicyVersion != policyVersion || s.PromptVersion != questionVersion {
				return fmt.Errorf("snapshot %s uses unsupported schema/policy/questions: %d %s %s", id, s.SchemaVersion, s.PolicyVersion, s.PromptVersion)
			}
			if opts.Shorts {
				policy.ShortSizePct = effective.Shorts.SizePct
			}
			var prompt struct {
				State     json.RawMessage         `json:"state"`
				Questions map[string]jev.Question `json:"questions"`
			}
			if err := json.Unmarshal([]byte(recorded.RawPrompt), &prompt); err != nil {
				return err
			}
			if len(prompt.State) == 0 || len(prompt.Questions) == 0 {
				return fmt.Errorf("snapshot %s lacks recorded state/questions", id)
			}
			if err := normalizeReplayQuestions(prompt.Questions); err != nil {
				return err
			}
			req := in.Request()
			expState, expQuestions, err := jev.EntryExperiment(req, policy, effective.JevMinRiskReward, opts)
			if err != nil {
				return err
			}
			version, _ := opts.Versions()
			for _, v := range []struct {
				name, version string
				state         any
				questions     map[string]jev.Question
			}{
				{"baseline", version, prompt.State, prompt.Questions},
				{jev.EntryExperimentVersion, jev.EntryExperimentVersion, expState, expQuestions},
			} {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s %s %s\n", id, s.Ticker, v.name)
				response, meta, callErr := client.Evaluate(cmd.Context(), v.state, v.questions, v.version)
				row := jevReplayResult{SnapshotID: id, Ticker: s.Ticker, Variant: v.name, RecordedModel: recorded.ResolvedModel, RecordedOutput: recorded.ParsedResponse, Meta: meta, Answers: response.Answers}
				if callErr == nil {
					callErr = jev.ValidateAnswers(v.questions, response.Answers)
				}
				if callErr == nil {
					// Experimental blocker questions are observational; resolve only
					// the original policy questions with the recorded thresholds.
					answers := map[string]jev.Answer{}
					for k := range jev.QuestionsWith(req, opts) {
						answers[k] = response.Answers[k]
					}
					out, trace, err := jev.ResolveWith(req, answers, policy, effective.JevMinRiskReward, opts)
					callErr = err
					if err == nil {
						row.Output = &out
						row.Trace = &trace
					}
				}
				if callErr != nil {
					row.Error = callErr.Error()
					failed = true
				}
				if err := enc.Encode(row); err != nil {
					return err
				}
				if err := f.Sync(); err != nil {
					return err
				}
				if cmd.Context().Err() != nil {
					return cmd.Context().Err()
				}
			}
		}
		if failed {
			return fmt.Errorf("one or more replay calls failed; paid-call evidence retained in %s", output)
		}
		return nil
	},
}

// Question.Criteria is an interface on the wire. Restore its typed shape so
// ValidateAnswers checks the recorded option set rather than an empty menu.
func normalizeReplayQuestions(questions map[string]jev.Question) error {
	for name, q := range questions {
		raw, err := json.Marshal(q.Criteria)
		if err != nil {
			return err
		}
		switch q.Type {
		case jev.TypeChoice, jev.TypeNoul:
			var options map[string]string
			if err := json.Unmarshal(raw, &options); err != nil {
				return fmt.Errorf("question %s: %w", name, err)
			}
			q.Criteria = options
		case jev.TypeScore:
			var levels []string
			if err := json.Unmarshal(raw, &levels); err != nil {
				return err
			}
			q.Criteria = levels
		default:
			return fmt.Errorf("question %s has unsupported type %s", name, q.Type)
		}
		questions[name] = q
	}
	return nil
}

func init() {
	jevReplayCmd.Flags().String("output", "", "New private JSONL file (must not already exist)")
	jevReplayCmd.Flags().String("model", "", "Requested model; defaults to configured alias, served versions recorded")
	rootCmd.AddCommand(jevReplayCmd)
}
