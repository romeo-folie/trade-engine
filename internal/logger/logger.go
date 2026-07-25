// Package logger provides a structured logger built on log/slog.
// Writes to both stdout and ~/.tradeengine/logs/engine.log simultaneously.
// The log file is rotated automatically via lumberjack (size, age, backup count).
package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	once    sync.Once
	std     *slog.Logger
	logFile *lumberjack.Logger
)

// RotationConfig carries log-file rotation settings.
type RotationConfig struct {
	MaxSizeMB  int  // max megabytes before rotation; default 50
	MaxBackups int  // max number of old log files to keep; default 7
	MaxAgeDays int  // max days to keep old log files; default 30
	Compress   bool // gzip rotated files; default true
}

// DefaultRotationConfig returns sensible defaults for rotation.
func DefaultRotationConfig() RotationConfig {
	return RotationConfig{MaxSizeMB: 50, MaxBackups: 7, MaxAgeDays: 30, Compress: true}
}

// Init initialises the global logger with rotation defaults.
// level: debug | info | warn | error. format: text | json.
func Init(level, format string) error {
	return InitWithRotation(level, format, DefaultRotationConfig())
}

// InitWithRotation initialises the global logger with explicit rotation settings.
func InitWithRotation(level, format string, rot RotationConfig) error {
	var initErr error
	once.Do(func() {
		initErr = initLogger(level, format, rot)
	})
	return initErr
}

func initLogger(level, format string, rot RotationConfig) error {
	slogLevel := parseLevel(level)

	// Ensure log directory exists.
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".tradeengine", "logs")
	if err := os.MkdirAll(logDir, 0750); err != nil {
		return err
	}

	logPath := filepath.Join(logDir, "engine.log")

	// Eagerly validate that the log file is writable. lumberjack opens lazily on
	// the first write, so without this check a permissions error would be silent.
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	f.Close()

	logFile = &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    rot.MaxSizeMB,
		MaxBackups: rot.MaxBackups,
		MaxAge:     rot.MaxAgeDays,
		Compress:   rot.Compress,
	}

	w := io.MultiWriter(os.Stdout, logFile)
	opts := &slog.HandlerOptions{Level: slogLevel}

	var handler slog.Handler
	if strings.ToLower(format) == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}

	std = slog.New(handler)
	slog.SetDefault(std)
	return nil
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Get returns the global slog.Logger.
// If Init/InitWithRotation was never called, it initialises with defaults via
// once.Do so that a subsequent Init call doesn't open a second writer.
func Get() *slog.Logger {
	// Always enter sync.Once. Checking std before Once.Do races with the
	// goroutine that is publishing std inside initLogger; Once.Do supplies the
	// required happens-before edge even after initialization has completed.
	once.Do(func() {
		_ = initLogger("info", "text", DefaultRotationConfig())
	})
	// Init can fail before publishing std (for example, an unwritable log
	// directory). Logging must remain safe even in that degraded state.
	if std == nil {
		return slog.Default()
	}
	return std
}

// LogFilePath returns the path to the engine log file.
func LogFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tradeengine", "logs", "engine.log")
}

// Close flushes and closes the underlying log file.
func Close() {
	if logFile != nil {
		_ = logFile.Close()
	}
}

// Convenience wrappers.

func Debug(msg string, args ...any) { Get().Debug(msg, args...) }
func Info(msg string, args ...any)  { Get().Info(msg, args...) }
func Warn(msg string, args ...any)  { Get().Warn(msg, args...) }
func Error(msg string, args ...any) { Get().Error(msg, args...) }

// SetForTesting replaces both the package-level logger (`std`, used by the
// Debug/Info/Warn/Error wrappers in this file) AND the slog default
// (`slog.Default`, used by callers that log via `slog.Info` etc. directly,
// such as the orb and scanner packages) with the supplied *slog.Logger, and
// returns a restore function that puts both back.
//
// Both must be set in lockstep: tests that capture WARN records would
// otherwise miss every record emitted via `slog.*` directly, producing false
// negatives where the production code clearly logged but the test asserted
// "no logs". Restore reverses both in the inverse order.
//
// Not safe for concurrent test use across goroutines unless the callers
// serialise themselves.
func SetForTesting(l *slog.Logger) (restore func()) {
	// Complete lazy initialization before swapping the logger. Otherwise the
	// first wrapper call would run once.Do, overwrite l, and leave std nil after
	// restore while the Once remained spent.
	prev := Get()
	prevDefault := slog.Default()
	std = l
	slog.SetDefault(l)
	return func() {
		std = prev
		slog.SetDefault(prevDefault)
	}
}
