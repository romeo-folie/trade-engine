package main

import (
	"context"
	"errors"
	"strings"
	"testing"
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
