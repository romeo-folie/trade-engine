package store

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

const failedConnectHelperEnv = "TRADE_ENGINE_TEST_FAILED_CONNECT_HELPER"

func TestConnectFailedPingLeavesStoreDisconnected(t *testing.T) {
	if os.Getenv(failedConnectHelperEnv) == "1" {
		Close()
		t.Setenv("DATABASE_URL", "postgres://127.0.0.1:1/tradeengine_connect_failure")

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := Connect(ctx); err == nil {
			t.Fatal("Connect() error = nil, want failed ping")
		}
		if IsConnected() {
			t.Fatal("IsConnected() = true after failed Connect, want false")
		}
		return
	}

	// Isolate the global store lifecycle from the integration-test connection
	// established by TestMain and from every other test in this package.
	cmd := exec.Command(os.Args[0], "-test.run=^TestConnectFailedPingLeavesStoreDisconnected$")
	cmd.Env = append(os.Environ(), failedConnectHelperEnv+"=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed-connect helper: %v\n%s", err, output)
	}
}

func TestConnectWhileConnectedPreservesPublishedPool(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set; store integration tests skipped")
	}

	db := GetDB()
	if err := Connect(context.Background()); err != nil {
		t.Fatalf("second Connect() error = %v", err)
	}
	if got := GetDB(); got != db {
		t.Fatal("second Connect() replaced the published pool")
	}

	var one int
	if err := db.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query through previously borrowed pool: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 = %d, want 1", one)
	}
}

func TestConnectRejectsDSNChangeWhileConnected(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set; store integration tests skipped")
	}

	db := GetDB()
	t.Setenv("DATABASE_URL", "postgres://different.invalid/tradeengine")
	if err := Connect(context.Background()); err == nil {
		t.Fatal("Connect() accepted a changed DATABASE_URL while connected")
	}
	if got := GetDB(); got != db {
		t.Fatal("rejected DSN change replaced the published pool")
	}

	var one int
	if err := db.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query through pool after rejected DSN change: %v", err)
	}
}

func TestConcurrentConnectAndBorrowedPoolUse(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set; store integration tests skipped")
	}

	db := GetDB()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers*2)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errs <- Connect(ctx)
		}()
		go func() {
			defer wg.Done()
			<-start
			var one int
			errs <- db.QueryRow(ctx, "SELECT 1").Scan(&one)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Connect/query: %v", err)
		}
	}
	if got := GetDB(); got != db {
		t.Fatal("concurrent Connect() replaced the published pool")
	}
}

func TestRetryPoliticalDisclosureReportAtUsesCallerClock(t *testing.T) {
	if !IsConnected() {
		t.Skip("DATABASE_URL not set; store integration tests skipped")
	}

	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		offset     time.Duration
		externalID string
		token      string
	}{
		{name: "app clock behind database", offset: -24 * time.Hour, externalID: "test-retry-caller-clock-behind", token: "caller-clock-behind"},
		{name: "app clock ahead database", offset: 24 * time.Hour, externalID: "test-retry-caller-clock-ahead", token: "caller-clock-ahead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := PoliticalDisclosureReport{
				Source: "senate", ExternalID: tc.externalID, Chamber: "senate",
				FilerName: "Clock Senator", ReportType: "periodic_transaction",
				FilingDate: time.Now().UTC(), DocumentURL: "https://example.test/caller-clock",
				ExtractionStatus: "failed", ExtractionError: "temporary", RetryableFailure: true,
			}
			t.Cleanup(func() {
				_, _ = GetDB().Exec(context.Background(),
					"DELETE FROM political_disclosure_reports WHERE source=$1 AND external_id=$2",
					report.Source, report.ExternalID)
			})
			if err := SavePoliticalDisclosureReport(ctx, report); err != nil {
				t.Fatal(err)
			}

			appNow := time.Now().UTC().Add(tc.offset)
			found, err := retryPoliticalDisclosureReportAt(ctx, report.Source, report.ExternalID, appNow)
			if err != nil || !found {
				t.Fatalf("manual retry found=%v err=%v", found, err)
			}
			terminal, err := PoliticalDisclosureReportTerminal(ctx, report.Source, report.ExternalID)
			if err != nil {
				t.Fatal(err)
			}
			if terminal {
				t.Fatal("manual retry is terminal immediately after reset")
			}
			claimed, err := ClaimPoliticalDisclosureRetries(ctx, report.Source, appNow, 1, time.Minute, tc.token)
			if err != nil || len(claimed) != 1 || claimed[0].ExternalID != report.ExternalID {
				t.Fatalf("immediate claim under skew = %+v, err=%v", claimed, err)
			}
		})
	}
}
