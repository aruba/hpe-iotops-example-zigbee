package logger

import (
	"log/slog"
	"os"
	"strings"
)

// New builds a JSON logger that writes to stdout.
// JSON output is easy to parse in container logs and observability tools.
func New(level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(level),
	}))
}

// parseLevel converts text from config (e.g. "INFO") to slog.Level.
// Unknown values default to INFO to avoid accidental silent logs.
func parseLevel(level string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
