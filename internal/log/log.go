package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	initOnce    sync.Once
	initialized atomic.Bool
)

func Setup(logFile string, debug bool, ws ...io.Writer) {
	initOnce.Do(func() {
		// Fork patch (concurrency): MaxBackups was 0 upstream, which
		// effectively disabled rotation — once a process reached MaxSize
		// the file grew indefinitely. Under parallel `crush run` the
		// shared log file balloons quickly. Keep 3 compressed backups so
		// rotation actually runs but disk usage stays bounded. See
		// CHANGELOG.fork.md.
		logRotator := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    10, // Max size in MB
			MaxBackups: 3,  // keep last 3 rotated files
			MaxAge:     30, // Days
			Compress:   true,
		}

		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}

		opts := &slog.HandlerOptions{
			Level:     level,
			AddSource: true,
		}

		// Tag every entry with this process's PID. When two crush
		// processes share a .crush dir (common with parallel `crush run
		// --session X` orchestration), the lumberjack file gets
		// interleaved writes from both. The pid attribute lets
		// post-hoc filtering split them cleanly: `jq 'select(.pid==N)'`.
		// Cheap (one int per log line) and harmless when there's only
		// one process.
		pid := os.Getpid()
		var handlers []slog.Handler
		handlers = append(handlers, slog.NewJSONHandler(logRotator, opts).WithAttrs([]slog.Attr{slog.Int("pid", pid)}))

		for _, w := range ws {
			if w == nil {
				continue
			}
			if f, ok := w.(term.File); ok && term.IsTerminal(f.Fd()) {
				handlers = append(handlers, slog.NewTextHandler(w, opts).WithAttrs([]slog.Attr{slog.Int("pid", pid)}))
			} else {
				handlers = append(handlers, slog.NewJSONHandler(w, opts).WithAttrs([]slog.Attr{slog.Int("pid", pid)}))
			}
		}

		slog.SetDefault(slog.New(slog.NewMultiHandler(handlers...)))
		initialized.Store(true)
	})
}

func Initialized() bool {
	return initialized.Load()
}

func RecoverPanic(name string, cleanup func()) {
	if r := recover(); r != nil {
		// Create a timestamped panic log file
		timestamp := time.Now().Format("20060102-150405")
		filename := fmt.Sprintf("crush-panic-%s-%s.log", name, timestamp)

		file, err := os.Create(filename)
		if err == nil {
			defer file.Close()

			// Write panic information and stack trace
			fmt.Fprintf(file, "Panic in %s: %v\n\n", name, r)
			fmt.Fprintf(file, "Time: %s\n\n", time.Now().Format(time.RFC3339))
			fmt.Fprintf(file, "Stack Trace:\n%s\n", debug.Stack())

			// Execute cleanup function if provided
			if cleanup != nil {
				cleanup()
			}
		}
	}
}
