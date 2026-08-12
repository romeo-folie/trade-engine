package stockorders

import (
	"strings"
	"testing"
)

func TestClientIDForKeyIsDeterministicAndBounded(t *testing.T) {
	first := clientIDForKey("entry:signal-123")
	if got := clientIDForKey("entry:signal-123"); got != first {
		t.Fatalf("same intent key produced %q then %q", first, got)
	}
	if got := clientIDForKey("entry:signal-456"); got == first {
		t.Fatal("different intent keys share client id")
	}
	if !strings.HasPrefix(first, clientIDPrefix) || len(first) > 128 {
		t.Fatalf("client id %q is not a valid bounded Alpaca key", first)
	}
}

func TestAnalystExitSignalIDAcceptsOnlyValidatedRetryLineage(t *testing.T) {
	const (
		signalID = "signal-123"
		ticker   = "SPY"
	)
	for _, tc := range []struct {
		name string
		key  string
		ok   bool
	}{
		{"base", "analyst-exit:signal-123:SPY", true},
		{"broker retry", "analyst-exit:signal-123:SPY:retry:broker-123", true},
		{"brokerless retry", "analyst-exit:signal-123:SPY:retry:intent-123", true},
		{"multiple retries", "analyst-exit:signal-123:SPY:retry:intent-123:retry:broker-456", true},
		{"missing signal", "analyst-exit::SPY", false},
		{"mismatched ticker", "analyst-exit:signal-123:QQQ", false},
		{"missing retry identity", "analyst-exit:signal-123:SPY:retry", false},
		{"empty retry identity", "analyst-exit:signal-123:SPY:retry:", false},
		{"unknown suffix", "analyst-exit:signal-123:SPY:replacement:broker-123", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := analystExitSignalID(tc.key, ticker)
			if ok != tc.ok || (ok && got != signalID) {
				t.Fatalf("analystExitSignalID(%q) = %q, %v; want %q, %v", tc.key, got, ok, signalID, tc.ok)
			}
		})
	}
}
