package engine

import "testing"

func TestProtectiveStopIntentKeyResizesWithoutPayloadReuse(t *testing.T) {
	first := protectiveStopIntentKey("trade-1", 90, 3)
	larger := protectiveStopIntentKey("trade-1", 90, 5)
	if first == larger {
		t.Fatalf("resize reused intent key %q", first)
	}
	if got := protectiveStopIntentKey("trade-1", 90, 5); got != larger {
		t.Fatalf("same resize not replay-stable: %q != %q", got, larger)
	}
}
