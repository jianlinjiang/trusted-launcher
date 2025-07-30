package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/jianlinjiang/trusted-launcher/metadata"
)

const (
	logName           = "trusted-launcher"
	serialConsoleFile = "/dev/console"

	payloadMessageKey      = "MESSAGE"
	payloadInstanceNameKey = "_HOSTNAME"
)

// Logger defines the interface for the CS image logger.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)

	SerialConsoleFile() *os.File
	Close()
}

type logger struct {
	serialLogger *slog.Logger

	instanceName      string
	serialConsoleFile *os.File
}

// NewLogger returns a Logger with Serial Console logging configured.
func NewLogger(ctx context.Context) (Logger, error) {
	instanceName, err := metadata.InstanceId()
	if err != nil {
		return nil, err
	}

	serialConsole, err := os.OpenFile(serialConsoleFile, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open serial console for writing: %v", err)
	}

	slg := slog.New(slog.NewTextHandler(serialConsole, nil))
	slg.Info("Serial Console logger initialized")

	// This is necessary for DEBUG logs to propagate properly.
	slog.SetDefault(slg)

	return &logger{
		serialLogger:      slg,
		instanceName:      instanceName,
		serialConsoleFile: serialConsole,
	}, err
}

func (l *logger) SerialConsoleFile() *os.File {
	return l.serialConsoleFile
}

func (l *logger) Close() {
	if l.serialConsoleFile != nil {
		l.serialConsoleFile.Close()
	}
}

func (l *logger) Info(msg string, args ...any) {
	l.serialLogger.Info(msg, args...)
}

func (l *logger) Warn(msg string, args ...any) {
	l.serialLogger.Warn(msg, args...)
}

func (l *logger) Error(msg string, args ...any) {
	l.serialLogger.Error(msg, args...)
}

// SimpleLogger returns a lightweight implementation that wraps a slog.Default() logger.
// Suitable for testing.
func SimpleLogger() Logger {
	return &slogger{slog.Default()}
}

type slogger struct {
	slg *slog.Logger
}

// Info logs msg and args at 'Info' severity.
func (l *slogger) Info(msg string, args ...any) {
	l.slg.Info(msg, args...)
}

// Warn logs msg and args at 'Warn' severity.
func (l *slogger) Warn(msg string, args ...any) {
	l.slg.Warn(msg, args...)
}

// Error logs msg and args at 'Error' severity.
func (l *slogger) Error(msg string, args ...any) {
	l.slg.Error(msg, args...)
}

func (l *slogger) SerialConsoleFile() *os.File {
	return nil
}

func (l *slogger) Close() {}
