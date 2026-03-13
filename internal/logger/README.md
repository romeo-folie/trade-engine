# internal/logger — Structured Logger

A thin wrapper around Go's standard `log/slog` that writes to **both stdout and a log file** simultaneously.

## Log Location

```
~/.tradeengine/logs/engine.log
```

The directory is created automatically on `Init()`. The file is opened in append mode; rotation is not handled by this package.

## Usage

```go
import "github.com/romeo-folie/trade-engine/internal/logger"

// Initialise once at startup (called automatically by `te run` via config).
if err := logger.Init("info", "text"); err != nil {
    logger.Error("logger init failed", "err", err)
    os.Exit(1)
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

Logging level and format are set in `config.yaml` and loaded via `config.Logging`:

```yaml
logging:
  level: info    # debug | info | warn | error
  format: text   # text | json
```

`te run` calls `logger.Init(cfg.Logging.Level, cfg.Logging.Format)` automatically at startup.

## Convenience Wrappers

```go
logger.Debug(msg, args...)
logger.Info(msg, args...)
logger.Warn(msg, args...)
logger.Error(msg, args...)
```

All wrappers call through to the global `slog.Logger`. If `Init()` was not called, the logger falls back to `info`/`text` defaults automatically.
