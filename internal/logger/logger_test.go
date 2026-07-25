package logger

import (
	"io"
	"log/slog"
	"testing"
)

func TestSetForTestingBeforeFirstLogSurvivesRestore(t *testing.T) {
	capture := slog.New(slog.NewTextHandler(io.Discard, nil))
	restore := SetForTesting(capture)

	if got := Get(); got != capture {
		t.Fatalf("Get() replaced test logger: got %p, want %p", got, capture)
	}

	restore()

	if got := Get(); got == nil {
		t.Fatal("Get() returned nil after restoring first test logger")
	}
	Info("logger remains usable after test restore")
}
