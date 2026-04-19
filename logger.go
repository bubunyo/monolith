package monolith

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Log level constants — numeric for comparison, string labels for output.
const (
	LvlTrace = iota
	LvlInfo
	LvlWarn
	LvlError
)

// Log level labels indexed by numeric level.
var levelLabels = [...]string{
	LvlTrace: "TRC",
	LvlInfo:  "INF",
	LvlWarn:  "WRN",
	LvlError: "ERR",
}

// sink is an output destination with a minimum log level.
type sink struct {
	w        io.Writer
	minLevel int
}

// Logger writes structured log lines with timestamps and levels.
// It supports multiple output sinks, each with an independent minimum level.
// It is safe for concurrent use.
type Logger struct {
	mu    sync.Mutex
	sinks []sink
}

// NewLogger creates a Logger that writes all levels to w.
func NewLogger(w io.Writer) *Logger {
	return &Logger{
		sinks: []sink{{w: w, minLevel: LvlTrace}},
	}
}

// AddSink adds an additional output writer that receives messages at or above minLevel.
func (l *Logger) AddSink(w io.Writer, minLevel int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sinks = append(l.sinks, sink{w: w, minLevel: minLevel})
}

// Info logs at INF level.
func (l *Logger) Info(format string, args ...any) {
	l.log(LvlInfo, format, args...)
}

// Warn logs at WRN level.
func (l *Logger) Warn(format string, args ...any) {
	l.log(LvlWarn, format, args...)
}

// Error logs at ERR level.
func (l *Logger) Error(format string, args ...any) {
	l.log(LvlError, format, args...)
}

// Trace logs at TRC level. Used for verbose/diagnostic output such as packet monitors.
func (l *Logger) Trace(format string, args ...any) {
	l.log(LvlTrace, format, args...)
}

func (l *Logger) log(level int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().Format("2006-01-02 15:04:05")
	label := "???"
	if level >= 0 && level < len(levelLabels) {
		label = levelLabels[level]
	}
	line := []byte(fmt.Sprintf("%s %s %s\n", ts, label, msg))

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.sinks {
		if level >= s.minLevel {
			s.w.Write(line)
		}
	}
}
