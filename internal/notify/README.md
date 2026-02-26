# notify

Telegram notification bot for TradeEngine.

## Overview

The `notify` package provides a Telegram bot that subscribes to the engine's
event bus and delivers formatted trade alerts, signal notifications, manual
approval buttons, daily summaries, and error/warning messages.

## Architecture

```
Event Bus ──► TelegramBot (goroutine per event type)
                 │
                 ├─ sendMessage → Telegram Bot API
                 └─ pollUpdates → getUpdates long-poll
                      └─ handleCallback → CallbackHandler (approve/reject/detail)
```

- **No external dependencies** — uses `net/http` + `encoding/json` for the Telegram Bot API.
- Runs as background goroutines within the engine process.
- Inline keyboard buttons for approve/reject/detail on pending signals.
- Respects `notifications.telegram.notify_on` config filter.

## Event → Notification Mapping

| Event Type | Notification | Config Key |
|---|---|---|
| `trade.executed` | Trade entry alert | `trade` |
| `trade.closed` | Trade exit with P&L | `trade` |
| `signal.generated` | New signal alert | `signal` |
| `signal.queued` | Pending approval + inline buttons | `signal` |
| `signal.blocked` | Block reason | `signal` |
| `error` | Error/warning alert | `error` |
| (scheduler) | Daily portfolio summary | `daily_summary` |

## Configuration

```yaml
notifications:
  telegram:
    enabled: true
    bot_token_env: TELEGRAM_BOT_TOKEN
    chat_id: "123456789"
    notify_on:
      - signal
      - trade
      - error
      - daily_summary
```

## Inline Approval Flow

In manual mode, `signal.queued` events produce messages with inline buttons:

- **✅ Approve** → executes the trade
- **❌ Reject** → cancels the signal
- **📋 Full Analysis** → sends expanded reasoning

Expired/stale signals are handled gracefully when buttons are pressed late.
