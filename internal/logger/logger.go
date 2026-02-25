// Package logger provides a structured logger built on log/slog.
// Writes to both stdout and ~/.tradeengine/logs/engine.log simultaneously.
package logger

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	once      sync.Once
	std       *slog.Logger
	logFile   *os.File
)

// Init initialises the global logger. Must be called once at startup.
// level: debug | info | warn | error. format: text | json.
func Init(level, format string) error {
	var initErr error
	once.Do(func() {
		initErr = initLogger(level, format)
	})
	return initErr
}

func initLogger(level, format string) error {
	slogLevel := parseLevel(level)

	// Ensure log directory exists.
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, ".tradeengine", "logs")
	if err := os.MkdirAll(logDir, 0750); err != nil {
		return err
	}

	var err error
	logFile, err = os.OpenFile(
		filepath.Join(logDir, "engine.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0640,
	)
	if err != nil {
		return err
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
func Get() *slog.Logger {
	if std == nil {
		// Fallback: initialise with defaults if Init wasn't called.
		_ = initLogger("info", "text")
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
		logFile.Close()
	}
}

// Convenience wrappers.

func Debug(msg string, args ...any)  { Get().Debug(msg, args...) }
func Info(msg string, args ...any)   { Get().Info(msg, args...) }
func Warn(msg string, args ...any)   { Get().Warn(msg, args...) }
func Error(msg string, args ...any)  { Get().Error(msg, args...) }
