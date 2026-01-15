package mono

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type FileLogger struct {
	file    *os.File
	start   time.Time
	envName string
}

func NewFileLogger(envName string) (*FileLogger, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}

	monoDir := filepath.Join(home, ".mono")
	if err := os.MkdirAll(monoDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create ~/.mono directory: %w", err)
	}

	logPath := filepath.Join(monoDir, "mono.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	return &FileLogger{
		file:    f,
		start:   time.Now(),
		envName: envName,
	}, nil
}

func (l *FileLogger) Log(format string, args ...any) {
	if l.file == nil {
		return
	}
	elapsed := time.Since(l.start)
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.file, "[%s] [+%v] [%s] %s\n",
		time.Now().Format("15:04:05.000"),
		elapsed.Round(time.Millisecond),
		l.envName,
		msg)
}

func (l *FileLogger) Close() {
	if l.file != nil {
		l.file.Close()
	}
}

type LogWriter struct {
	logger  *FileLogger
	stream  string
}

func NewLogWriter(logger *FileLogger, stream string) *LogWriter {
	return &LogWriter{
		logger: logger,
		stream: stream,
	}
}

func (w *LogWriter) Write(p []byte) (n int, err error) {
	lines := strings.Split(string(p), "\n")
	for _, line := range lines {
		if line != "" {
			w.logger.Log("[%s] %s", w.stream, line)
		}
	}
	return len(p), nil
}

type ProgressLogger struct {
	logger      *FileLogger
	operation   string
	total       int64
	completed   atomic.Int64
	lastLogTime time.Time
	interval    time.Duration
	mu          sync.Mutex
}

func NewProgressLogger(logger *FileLogger, operation string, total int64) *ProgressLogger {
	return &ProgressLogger{
		logger:      logger,
		operation:   operation,
		total:       total,
		lastLogTime: time.Now(),
		interval:    5 * time.Second,
	}
}

func (p *ProgressLogger) Increment() {
	p.completed.Add(1)
	p.maybeLog()
}

func (p *ProgressLogger) maybeLog() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if time.Since(p.lastLogTime) < p.interval {
		return
	}

	p.logProgress()
	p.lastLogTime = time.Now()
}

func (p *ProgressLogger) logProgress() {
	completed := p.completed.Load()
	if p.total > 0 {
		pct := float64(completed) / float64(p.total) * 100
		p.logger.Log("%s: %d/%d files (%.0f%%)", p.operation, completed, p.total, pct)
	} else {
		p.logger.Log("%s: %d files", p.operation, completed)
	}
}

func (p *ProgressLogger) Done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logProgress()
}
