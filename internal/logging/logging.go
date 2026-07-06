// Package logging configures the shared *slog.Logger used across the hub.
//
// Logs are written to BOTH stdout and a rotating log file. Format is JSON by
// default but can be switched to text via config.
package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Setup opens the log file, wires slog to stdout + file, and returns the file
// as an io.Closer for the caller to defer.
func Setup(logPath, level, format string) (io.Closer, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	writer := io.MultiWriter(os.Stdout, f)
	handler := buildHandler(writer, level, format)
	slog.SetDefault(slog.New(handler))
	return f, nil
}

func buildHandler(w io.Writer, level, format string) slog.Handler {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	switch strings.ToLower(format) {
	case "text":
		return slog.NewTextHandler(w, opts)
	default:
		return slog.NewJSONHandler(w, opts)
	}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
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
