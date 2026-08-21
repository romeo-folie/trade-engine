package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestRenewPoliticalDisclosureRetryLeaseValidatesOwnershipInputs(t *testing.T) {
	for _, test := range []struct {
		name  string
		token string
		lease time.Duration
	}{
		{name: "blank token", lease: time.Minute},
		{name: "zero lease", token: "token"},
		{name: "negative lease", token: "token", lease: -time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			ok, err := store.RenewPoliticalDisclosureRetryLease(context.Background(), "senate", "report", test.token, time.Now(), test.lease)
			if err == nil || ok {
				t.Fatalf("ok=%v err=%v, want input validation failure", ok, err)
			}
		})
	}
}
