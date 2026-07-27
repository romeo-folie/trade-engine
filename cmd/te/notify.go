package main

import (
	"context"
	"fmt"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/notify"
	"github.com/spf13/cobra"
)

var notifyCmd = &cobra.Command{
	Use:   "notify",
	Short: "Notification commands",
}

type telegramImmediateSender interface {
	SendImmediate(context.Context, string) error
}

func sendTelegramConnectivityTest(
	ctx context.Context, sender telegramImmediateSender,
) error {
	if err := sender.SendImmediate(
		ctx, "🧪 *TradeEngine Telegram connectivity test*",
	); err != nil {
		return fmt.Errorf("send Telegram connectivity test: %w", err)
	}
	return nil
}

var notifyTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Send a test message to Telegram to verify bot connectivity",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if !cfg.Notifications.Telegram.Enabled {
			return fmt.Errorf("telegram notifications are not enabled in config")
		}
		if cfg.Notifications.Telegram.BotToken == "" {
			return fmt.Errorf("telegram bot token not set (env: %s)", cfg.Notifications.Telegram.BotTokenEnv)
		}
		if cfg.Notifications.Telegram.ChatID == "" {
			return fmt.Errorf("telegram chat_id not set in config")
		}

		bot := notify.NewTelegramBot(cfg.Notifications.Telegram, nil)
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		if err := sendTelegramConnectivityTest(ctx, bot); err != nil {
			return err
		}
		fmt.Println("✅ Test message sent. Check your Telegram chat.")
		return nil
	},
}

func init() {
	notifyCmd.AddCommand(notifyTestCmd)
}
