package execution

import (
	"strings"
	"testing"
	"time"
)

func TestValidateBrokerAccountRejectsUnexpectedAccountNumber(t *testing.T) {
	now := time.Now().UTC()
	account := BrokerAccount{
		AccountID: "account-id", AccountNumber: "connected-account", Status: "ACTIVE", Currency: "USD",
		Equity: 1_000, LastEquity: 1_000, Cash: 1_000, BuyingPower: 1_000, Multiplier: 1,
		CashAvailable: true, BuyingPowerAvailable: true, MultiplierAvailable: true, LastEquityAvailable: true,
		FetchedAt: now,
	}
	err := ValidateBrokerAccount(account, "expected-account", now)
	if err == nil || !strings.Contains(err.Error(), "expected_account_number") {
		t.Fatalf("ValidateBrokerAccount() error = %v, want expected account mismatch", err)
	}
}
