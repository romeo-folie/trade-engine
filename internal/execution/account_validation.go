package execution

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	accountSnapshotMaxAge          = 15 * time.Second
	accountSnapshotFutureTolerance = 10 * time.Second
)

// ValidateBrokerAccount verifies the account invariants required before a
// stock BUY can be admitted. It is shared by final entry admission and
// operator readiness so status never reports an account as usable when a BUY
// would fail closed.
func ValidateBrokerAccount(account BrokerAccount, expectedAccountNumber string, now time.Time) error {
	if account.FetchedAt.IsZero() {
		return fmt.Errorf("account snapshot timestamp is missing")
	}
	age := now.Sub(account.FetchedAt)
	if age > accountSnapshotMaxAge || age < -accountSnapshotFutureTolerance {
		return fmt.Errorf("account snapshot is stale or future-dated")
	}
	if !strings.EqualFold(account.Status, "active") || !strings.EqualFold(account.Currency, "USD") ||
		account.TradingBlocked || account.AccountBlocked || account.TradeSuspendedByUser {
		return fmt.Errorf("account is not active, unrestricted, and USD-denominated")
	}
	if strings.TrimSpace(account.AccountID) == "" || strings.TrimSpace(account.AccountNumber) == "" {
		return fmt.Errorf("account identity is missing")
	}
	if expected := strings.TrimSpace(expectedAccountNumber); expected != "" && account.AccountNumber != expected {
		return fmt.Errorf("connected account does not match expected_account_number")
	}
	if !account.CashAvailable || !account.BuyingPowerAvailable || !account.MultiplierAvailable ||
		!finiteNonNegative(account.Equity) || !finite(account.Cash) ||
		!finiteNonNegative(account.BuyingPower) || !finite(account.Multiplier) || account.Multiplier <= 0 {
		return fmt.Errorf("account equity, cash, buying power, or multiplier is invalid")
	}
	if !account.LastEquityAvailable || !finiteNonNegative(account.LastEquity) {
		return fmt.Errorf("account last_equity is missing or invalid")
	}
	return nil
}

func finite(v float64) bool            { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func finiteNonNegative(v float64) bool { return finite(v) && v >= 0 }
