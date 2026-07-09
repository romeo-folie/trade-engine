package analysis

import "testing"

// The whole point of the shared limiter: two clients of the SAME model reuse one
// limiter (so their rates don't add up past the per-model quota), while a
// different model gets its own.
func TestSharedModelRateLimiter_SameKeyReused(t *testing.T) {
	a := sharedModelRateLimiter("google:gemini-2.5-flash", 10)
	b := sharedModelRateLimiter("google:gemini-2.5-flash", 10)
	if a == nil || a != b {
		t.Fatalf("same model must share one limiter instance (a=%p b=%p)", a, b)
	}
	c := sharedModelRateLimiter("google:gemini-2.5-pro", 10)
	if c == a {
		t.Error("different models must not share a limiter")
	}
}

func TestSharedModelRateLimiter_DisabledWhenNonPositive(t *testing.T) {
	if l := sharedModelRateLimiter("google:whatever", 0); l != nil {
		t.Errorf("rpm 0 must disable pacing (nil), got %p", l)
	}
}
