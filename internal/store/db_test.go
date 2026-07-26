package store

import (
	"context"
	"os"
	"os/exec"
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
