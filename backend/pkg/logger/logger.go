// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package logger

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Default logger instance
var defaultLogger *slog.Logger

// Level represents a log level
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Init initializes the default logger with the specified log level
// If level is empty, it reads from LOG_LEVEL environment variable
// Defaults to INFO if not set
func Init(level string) {
	if level == "" {
		level = os.Getenv("LOG_LEVEL")
	}
	if level == "" {
		level = string(LevelInfo)
	}

	level = strings.ToLower(strings.TrimSpace(level))
	var slogLevel slog.Level

	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn", "warning":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: slogLevel,
	}

	// Use JSON handler for structured logging
	handler := slog.NewJSONHandler(os.Stdout, opts)
	defaultLogger = slog.New(handler)
}

// Get returns the default logger instance
func Get() *slog.Logger {
	if defaultLogger == nil {
		// Initialize with default level if not already initialized
		Init("")
	}
	return defaultLogger
}

// Debug logs a debug message
func Debug(msg string, args ...any) {
	Get().Debug(msg, args...)
}

// Info logs an info message
func Info(msg string, args ...any) {
	Get().Info(msg, args...)
}

// Warn logs a warning message
func Warn(msg string, args ...any) {
	Get().Warn(msg, args...)
}

// Error logs an error message
func Error(msg string, args ...any) {
	Get().Error(msg, args...)
}

// Debugf logs a formatted debug message
func Debugf(format string, args ...any) {
	Get().Debug(fmt.Sprintf(format, args...))
}

// Infof logs a formatted info message
func Infof(format string, args ...any) {
	Get().Info(fmt.Sprintf(format, args...))
}

// Warnf logs a formatted warning message
func Warnf(format string, args ...any) {
	Get().Warn(fmt.Sprintf(format, args...))
}

// Errorf logs a formatted error message
func Errorf(format string, args ...any) {
	Get().Error(fmt.Sprintf(format, args...))
}

// Fatal logs an error message and exits with status 1
func Fatal(msg string, args ...any) {
	Get().Error(msg, args...)
	os.Exit(1)
}

// Fatalf logs a formatted error message and exits with status 1
func Fatalf(format string, args ...any) {
	Get().Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// Print logs an info message (for compatibility with standard log package)
func Print(v ...any) {
	Get().Info(fmt.Sprint(v...))
}

// Printf logs a formatted info message (for compatibility with standard log package)
func Printf(format string, v ...any) {
	Get().Info(fmt.Sprintf(format, v...))
}

// Println logs an info message with newline (for compatibility with standard log package)
func Println(v ...any) {
	Get().Info(fmt.Sprintln(v...))
}
