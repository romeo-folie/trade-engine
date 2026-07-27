package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

type stubTelegramImmediateSender struct {
	err     error
	message string
}

func (s *stubTelegramImmediateSender) SendImmediate(
	_ context.Context, message string,
) error {
	s.message = message
	return s.err
}

func TestSendTelegramConnectivityTestIsUngatedAndReturnsSendError(t *testing.T) {
	sender := &stubTelegramImmediateSender{}
	if err := sendTelegramConnectivityTest(context.Background(), sender); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sender.message, "connectivity test") {
		t.Fatalf("connectivity message = %q", sender.message)
	}

	sentinel := errors.New("telegram rejected request")
	sender.err = sentinel
	err := sendTelegramConnectivityTest(context.Background(), sender)
	if !errors.Is(err, sentinel) {
		t.Fatalf("send error = %v; want wrapped sentinel", err)
	}
}

func TestValidateTelegramConnectivityConfigDoesNotRequireDeliveryEnabled(t *testing.T) {
	cfg := config.TelegramConfig{
		Enabled: false, BotToken: "token", ChatID: "chat",
	}
	if err := validateTelegramConnectivityConfig(cfg); err != nil {
		t.Fatalf("disabled delivery blocked connectivity test: %v", err)
	}

	cfg.BotToken = ""
	cfg.BotTokenEnv = "TELEGRAM_TOKEN"
	if err := validateTelegramConnectivityConfig(cfg); err == nil ||
		!strings.Contains(err.Error(), "TELEGRAM_TOKEN") {
		t.Fatalf("missing-token error = %v", err)
	}
}
