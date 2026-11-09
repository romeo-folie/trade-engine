// Package jev integrates TypeSafe's Jev (System One) model as a sole decision
// maker (issue #590). Jev evaluates typed questions against a state and
// returns probabilities rather than prose; this package owns the HTTP client,
// the versioned question set, and the deterministic policy that composes the
// typed answers into an engine-compatible decision.
//
// Nothing here executes orders or reads live state: the engine supplies the
// frozen analysis request and remains responsible for risk checks, entry
// gates, and protective exits.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/analysis"
)

// ProviderName identifies Jev calls in metadata, snapshots, and cost tracking.
const ProviderName = "typesafe"

// DefaultBaseURL is TypeSafe's public API root.
const DefaultBaseURL = "https://api.typesafe.ai"

// Question types supported by System One.
const (
	TypeChoice = "choice"
	TypeScore  = "score"
	TypeNoul   = "noul"
)

// Question is one typed System One question. Criteria is a map of choice name
// to description for choice questions, an ordered slice of level descriptions
// for score questions, and an optional {true,false} description map for noul.
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is one typed System One answer.
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Noul          *float64           `json:"noul,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// Response is a parsed System One response.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ClientConfig configures a Client. Model is required and should name a
// specific model rather than a moving alias so decisions stay comparable.
type ClientConfig struct {
	BaseURL    string
	APIKey     string
	Model      string
	Timeout    time.Duration // per HTTP attempt
	MaxRetries int           // transient retries after the first attempt
	// Backoff is the base delay between retries (doubled each retry).
	Backoff time.Duration
}

// maxRetries and maxBackoff bound retry cost and keep the doubling delay far
// from overflow.
const (
	maxRetries = 5
	maxBackoff = 30 * time.Second
)

// Client calls TypeSafe System One.
type Client struct {
	cfg  ClientConfig
	http *http.Client
}

// NewClient validates cfg and builds a client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("jev: model is required")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("jev: API key is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.MaxRetries > maxRetries {
		cfg.MaxRetries = maxRetries
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 500 * time.Millisecond
	}
	return &Client{cfg: cfg, http: &http.Client{}}, nil
}

// SetHTTPClient replaces the transport (tests).
func (c *Client) SetHTTPClient(h *http.Client) { c.http = h }

// Model returns the configured (requested) model.
func (c *Client) Model() string { return c.cfg.Model }

// APIError is a non-2xx System One response. Body is truncated and never
// contains request headers, so it is safe to log.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("typesafe API %d: %s", e.StatusCode, e.Body)
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Evaluate sends one System One request. It always returns the attempted
// call's metadata (identity, request, retries) so failures stay auditable;
// RawResponse and token counts are set only on success. state must marshal
// to JSON; questions must be non-empty.
func (c *Client) Evaluate(ctx context.Context, state any, questions map[string]Question, questionSetVersion string) (Response, analysis.ModelCallMeta, error) {
	meta := analysis.ModelCallMeta{
		Provider: ProviderName, Model: c.cfg.Model,
		RequestParams: map[string]any{"model": c.cfg.Model, "question_set_version": questionSetVersion},
	}
	if len(questions) == 0 {
		return Response{}, meta, errors.New("jev: at least one question is required")
	}
	body, err := json.Marshal(map[string]any{"model": c.cfg.Model, "state": state, "questions": questions})
	if err != nil {
		return Response{}, meta, fmt.Errorf("jev: marshal request: %w", err)
	}
	// The full request (state and questions) is the "prompt" of this call.
	meta.RawPrompt = string(body)
	meta.PromptHash = analysis.HashPrompt(meta.RawPrompt)

	url := c.cfg.BaseURL + "/v1/systemone"
	for attempt := 0; ; attempt++ {
		respBody, status, latency, err := c.do(ctx, url, body)
		meta.LatencyMs = latency
		if err == nil && status < 300 {
			// Keep the body even if it fails to parse: it is the evidence.
			meta.RawResponse = string(respBody)
			var parsed Response
			if err := json.Unmarshal(respBody, &parsed); err != nil {
				return Response{}, meta, fmt.Errorf("jev: parse response: %w", err)
			}
			meta.ResolvedModel = parsed.Model
			meta.InputTokens, meta.OutputTokens = parsed.Usage.InputTokens, parsed.Usage.OutputTokens
			return parsed, meta, nil
		}
		var callErr error
		if err != nil {
			callErr = err
		} else {
			callErr = &APIError{StatusCode: status, Body: truncate(string(bytes.TrimSpace(respBody)), 512)}
		}
		// Transport failures (including a per-attempt timeout) and transient
		// statuses are retried while the caller's context is still live.
		canRetry := attempt < c.cfg.MaxRetries && ctx.Err() == nil && (err != nil || retryable(status))
		if !canRetry {
			return Response{}, meta, callErr
		}
		delay := c.cfg.Backoff << attempt
		if delay > maxBackoff || delay <= 0 {
			delay = maxBackoff
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return Response{}, meta, callErr
		}
		select {
		case <-ctx.Done():
			return Response{}, meta, fmt.Errorf("%w (retry abandoned: %w)", callErr, ctx.Err())
		case <-time.After(delay):
		}
		meta.Retries = append(meta.Retries, analysis.TransportAttempt{
			StatusCode: status, Error: callErr.Error(), LatencyMs: latency,
		})
	}
}

func (c *Client) do(ctx context.Context, url string, body []byte) ([]byte, int, int64, error) {
	actx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("jev: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	start := time.Now()
	resp, err := c.http.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		// Never wrap the *url.Error's request: report only the cause.
		return nil, 0, latency, fmt.Errorf("jev: request failed: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, latency, fmt.Errorf("jev: read response: %w", err)
	}
	return respBody, resp.StatusCode, latency, nil
}

func unwrapURLError(err error) error {
	var u interface{ Unwrap() error }
	if errors.As(err, &u) {
		if inner := u.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ─── Answer validation ───────────────────────────────────────────────────────

// ErrMalformed marks a response that does not satisfy the question contract.
var ErrMalformed = errors.New("jev: malformed answer")

// probabilityTolerance bounds how far a distribution may sum from 1.
const probabilityTolerance = 0.02

func finiteUnit(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }

// ValidateAnswers checks that every question was answered with the right
// type, known options, finite unit-interval probabilities summing to ~1, and
// in-range scores. Any violation is ErrMalformed: the policy never guesses.
func ValidateAnswers(questions map[string]Question, answers map[string]Answer) error {
	for name, q := range questions {
		a, ok := answers[name]
		if !ok {
			return fmt.Errorf("%w: question %q unanswered", ErrMalformed, name)
		}
		if a.Type != q.Type {
			return fmt.Errorf("%w: question %q answered as %q, want %q", ErrMalformed, name, a.Type, q.Type)
		}
		switch q.Type {
		case TypeNoul:
			if a.Noul == nil || !finiteUnit(*a.Noul) {
				return fmt.Errorf("%w: question %q noul missing or out of [0,1]", ErrMalformed, name)
			}
		case TypeChoice:
			options, _ := q.Criteria.(map[string]string)
			if err := validateDistribution(name, a.Probabilities, func(k string) bool { _, ok := options[k]; return ok }, len(options)); err != nil {
				return err
			}
			if _, ok := options[a.Choice]; !ok {
				return fmt.Errorf("%w: question %q chose unknown option %q", ErrMalformed, name, a.Choice)
			}
			// The choice must be the distribution's most probable option; a
			// choice contradicting its own probabilities is not trusted.
			chosen, ok := a.Probabilities[a.Choice]
			if !ok {
				return fmt.Errorf("%w: question %q choice %q has no probability", ErrMalformed, name, a.Choice)
			}
			for opt, p := range a.Probabilities {
				if p > chosen+1e-9 {
					return fmt.Errorf("%w: question %q chose %q (%.3f) over more probable %q (%.3f)", ErrMalformed, name, a.Choice, chosen, opt, p)
				}
			}
			if a.Confidence == nil || !finiteUnit(*a.Confidence) {
				return fmt.Errorf("%w: question %q confidence missing or out of [0,1]", ErrMalformed, name)
			}
		case TypeScore:
			levels, _ := q.Criteria.([]string)
			if err := validateDistribution(name, a.Probabilities, func(k string) bool {
				i, err := strconv.Atoi(k)
				return err == nil && i >= 0 && i < len(levels)
			}, len(levels)); err != nil {
				return err
			}
			if a.Score == nil || math.IsNaN(*a.Score) || *a.Score < 0 || *a.Score > float64(len(levels)-1) {
				return fmt.Errorf("%w: question %q score missing or out of range", ErrMalformed, name)
			}
		default:
			return fmt.Errorf("%w: question %q has unsupported type %q", ErrMalformed, name, q.Type)
		}
	}
	return nil
}

func validateDistribution(name string, probs map[string]float64, known func(string) bool, want int) error {
	if len(probs) == 0 || len(probs) > want {
		return fmt.Errorf("%w: question %q has %d probabilities for %d options", ErrMalformed, name, len(probs), want)
	}
	var sum float64
	for k, p := range probs {
		if !known(k) {
			return fmt.Errorf("%w: question %q has probability for unknown option %q", ErrMalformed, name, k)
		}
		if !finiteUnit(p) {
			return fmt.Errorf("%w: question %q probability %q out of [0,1]", ErrMalformed, name, k)
		}
		sum += p
	}
	if math.Abs(sum-1) > probabilityTolerance {
		return fmt.Errorf("%w: question %q probabilities sum to %.4f", ErrMalformed, name, sum)
	}
	return nil
}
