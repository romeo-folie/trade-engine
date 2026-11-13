package main

import (
	"encoding/json"
	"github.com/romeo-folie/trade-engine/internal/jev"
	"testing"
)

func TestReplayRestoresRecordedQuestionTypes(t *testing.T) {
	var questions map[string]jev.Question
	if err := json.Unmarshal([]byte(`{"entry_timing":{"type":"choice","criteria":{"enter_now":"Enter","wait_for_better_entry":"Wait"}}}`), &questions); err != nil {
		t.Fatal(err)
	}
	if err := normalizeReplayQuestions(questions); err != nil {
		t.Fatal(err)
	}
	confidence := .8
	answers := map[string]jev.Answer{"entry_timing": {Type: jev.TypeChoice, Choice: "enter_now", Confidence: &confidence, Probabilities: map[string]float64{"enter_now": .8, "wait_for_better_entry": .2}}}
	if err := jev.ValidateAnswers(questions, answers); err != nil {
		t.Fatal(err)
	}
	answers["entry_timing"] = jev.Answer{Type: jev.TypeChoice, Choice: "invented", Confidence: &confidence, Probabilities: map[string]float64{"invented": 1}}
	if err := jev.ValidateAnswers(questions, answers); err == nil {
		t.Fatal("unknown recorded option accepted")
	}
}
