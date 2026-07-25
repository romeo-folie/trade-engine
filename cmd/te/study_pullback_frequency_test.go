package main

import (
	"testing"
	"time"
)

func TestPullbackFrequencyDayNormalizesTimeOfDay(t *testing.T) {
	location := time.FixedZone("study", -4*60*60)
	input := time.Date(2026, 7, 25, 18, 42, 13, 99, location)
	got := pullbackFrequencyDay(input)
	want := time.Date(2026, 7, 25, 0, 0, 0, 0, location)
	if !got.Equal(want) {
		t.Fatalf("pullbackFrequencyDay() = %s, want %s", got, want)
	}
}
