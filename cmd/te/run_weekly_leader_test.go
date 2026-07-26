package main

import "testing"

func TestShouldStartWeeklyLeaderScheduler(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		dbConnected bool
		want        bool
	}{
		{
			name:        "enabled with database",
			enabled:     true,
			dbConnected: true,
			want:        true,
		},
		{
			name:        "enabled without database",
			enabled:     true,
			dbConnected: false,
			want:        false,
		},
		{
			name:        "disabled with database",
			enabled:     false,
			dbConnected: true,
			want:        false,
		},
		{
			name:        "disabled without database",
			enabled:     false,
			dbConnected: false,
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldStartWeeklyLeaderScheduler(
				tt.enabled, tt.dbConnected,
			); got != tt.want {
				t.Fatalf(
					"shouldStartWeeklyLeaderScheduler(%t, %t) = %t; want %t",
					tt.enabled, tt.dbConnected, got, tt.want,
				)
			}
		})
	}
}
