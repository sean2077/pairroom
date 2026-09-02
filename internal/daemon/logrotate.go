package daemon

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	DefaultLogMaxSize        = 10 * 1024 * 1024
	DefaultLogMaxBackups     = 3
	LogFileEnvironment       = "PAIRROOM_LOG_FILE"
	LogSizeEnvironment       = "PAIRROOM_LOG_MAX_SIZE"
	LogBackupEnvironment     = "PAIRROOM_LOG_MAX_BACKUPS"
	ConsoleDetachEnvironment = "PAIRROOM_DETACH_CONSOLE"
)

type RotatingWriter struct {
	mu         sync.Mutex
	file       *os.File
	path       string
	maxSize    int64
	maxBackups int
	size       int64
}

func NewRotatingWriter(path string, maxSize int64, maxBackups int) (*RotatingWriter, error) {
	if maxSize < 1 {
		return nil, errors.New("log maximum size must be positive")
	}
	if maxBackups < 1 {
		return nil, errors.New("log backup count must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon log: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("protect daemon log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat daemon log: %w", err)
	}
	return &RotatingWriter{file: file, path: path, maxSize: maxSize, maxBackups: maxBackups, size: info.Size()}, nil
}

func (w *RotatingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	written, err := w.file.Write(data)
	w.size += int64(written)
	if w.size > w.maxSize {
		if rotateErr := w.rotateLocked(); err == nil {
			err = rotateErr
		}
	}
	return written, err
}

func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *RotatingWriter) rotateLocked() (resultErr error) {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	defer func() {
		if w.file != nil {
			return
		}
		file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("reopen daemon log: %w", err))
			return
		}
		w.file = file
		if info, err := file.Stat(); err == nil {
			w.size = info.Size()
		}
	}()
	oldest := w.backupPath(w.maxBackups)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove oldest log backup: %w", err)
	}
	for index := w.maxBackups - 1; index >= 1; index-- {
		source := w.backupPath(index)
		destination := w.backupPath(index + 1)
		if err := os.Rename(source, destination); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("shift log backup: %w", err)
		}
	}
	if err := os.Rename(w.path, w.backupPath(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate daemon log: %w", err)
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen daemon log: %w", err)
	}
	w.file = file
	w.size = 0
	return nil
}

func (w *RotatingWriter) backupPath(index int) string {
	return fmt.Sprintf("%s.%d", w.path, index)
}

func ParseLogSize(value string) (int64, error) {
	original := value
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("log size is empty")
	}
	upper := strings.ToUpper(value)
	multiplier := int64(1)
	for _, suffix := range []struct {
		name       string
		multiplier int64
	}{
		{"TB", 1024 * 1024 * 1024 * 1024}, {"T", 1024 * 1024 * 1024 * 1024},
		{"GB", 1024 * 1024 * 1024}, {"G", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024}, {"M", 1024 * 1024},
		{"KB", 1024}, {"K", 1024}, {"B", 1},
	} {
		if strings.HasSuffix(upper, suffix.name) {
			multiplier = suffix.multiplier
			value = strings.TrimSpace(value[:len(value)-len(suffix.name)])
			break
		}
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 1 {
		return 0, fmt.Errorf("invalid log size %q", original)
	}
	if number > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("log size %q overflows int64", original)
	}
	return number * multiplier, nil
}

func ParseLogBackups(value string) (int, error) {
	count, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || count < 1 || count > 1000 {
		return 0, fmt.Errorf("invalid log backup count %q", value)
	}
	return count, nil
}

func ConfigureProcessLoggingFromEnvironment() (func() error, error) {
	if daemonConsoleDetachRequested(os.Args) {
		detachOwnedWindowsConsole()
	}
	path := strings.TrimSpace(os.Getenv(LogFileEnvironment))
	if path == "" {
		return func() error { return nil }, nil
	}
	maxSize := int64(DefaultLogMaxSize)
	if value := os.Getenv(LogSizeEnvironment); value != "" {
		parsed, err := ParseLogSize(value)
		if err != nil {
			return nil, err
		}
		maxSize = parsed
	}
	maxBackups := DefaultLogMaxBackups
	if value := os.Getenv(LogBackupEnvironment); value != "" {
		parsed, err := ParseLogBackups(value)
		if err != nil {
			return nil, err
		}
		maxBackups = parsed
	}
	writer, err := NewRotatingWriter(path, maxSize, maxBackups)
	if err != nil {
		return nil, err
	}
	reader, pipeWriter, err := os.Pipe()
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("create daemon log pipe: %w", err)
	}
	originalStdout, originalStderr := os.Stdout, os.Stderr
	originalLogger := slog.Default()
	os.Stdout, os.Stderr = pipeWriter, pipeWriter
	slog.SetDefault(slog.New(slog.NewTextHandler(pipeWriter, nil)))
	done := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(writer, reader)
		done <- copyErr
	}()
	var once sync.Once
	var closeErr error
	cleanup := func() error {
		once.Do(func() {
			os.Stdout, os.Stderr = originalStdout, originalStderr
			slog.SetDefault(originalLogger)
			closeErr = errors.Join(pipeWriter.Close(), <-done, reader.Close(), writer.Close())
		})
		return closeErr
	}
	return cleanup, nil
}

func daemonConsoleDetachRequested(args []string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ConsoleDetachEnvironment))) {
	case "1", "true", "yes":
		return true
	}
	for _, argument := range args {
		if argument == "--daemon-control-file" || strings.HasPrefix(argument, "--daemon-control-file=") {
			return true
		}
	}
	return false
}
