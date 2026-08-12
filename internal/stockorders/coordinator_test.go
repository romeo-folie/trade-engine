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
