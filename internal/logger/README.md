# internal/logger — Structured Logger

A thin wrapper around Go's standard `log/slog` that writes to **both stdout and a rotating log file** simultaneously.

## Log Location

```
~/.tradeengine/logs/engine.log
```

The directory is created automatically on `Init()`.

## Usage

```go
import "github.com/romeo-folie/trade-engine/internal/logger"

// Initialise once at startup (main.go / cobra PersistentPreRun).
if err := logger.Init("info", "text"); err != nil {
    log.Fatal(err)
}
defer logger.Close()

// Structured log calls.
logger.Info("engine started", "mode", "paper", "tickers", 3)
logger.Warn("rate limit hit", "provider", "openai", "retry_in_ms", 2000)
logger.Error("trade failed", "ticker", "AAPL", "err", err)
```

## Levels

| Level | Value |
|---|---|
| `debug` | `slog.LevelDebug` |
| `info` *(default)* | `slog.LevelInfo` |
| `warn` / `warning` | `slog.LevelWarn` |
| `error` | `slog.LevelError` |

## Formats

| Format | Handler |
|---|---|
| `text` *(default)* | `slog.NewTextHandler` — human-readable key=value |
| `json` | `slog.NewJSONHandler` — machine-parseable |

## Configuration

Controlled via CLI flags (set in `cmd/te/root.go`):

```bash
te run --log-level debug --log-format json
```

## Convenience Wrappers

```go
logger.Debug(msg, args...)
logger.Info(msg, args...)
logger.Warn(msg, args...)
logger.Error(msg, args...)
```

All wrappers call through to the global `slog.Logger`. If `Init()` was not called, the logger falls back to `info`/`text` defaults automatically.
