package logger

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"
)

// Setup configures a logger for writing to a given file, providing a proper cleanup function
func Setup(logPath string, dryRun bool) (func(), error) {
	// dynamically build up log file name if non-dry-run so that each run is a different filename, based on a timestamp
	if !dryRun {
		logPath = path.Join(path.Dir(logPath), fmt.Sprintf("%s_%s.log",
			strings.TrimSuffix(path.Base(logPath), ".log"), getTimestamp()))
	}
	// write to stderr to tell user where log is being written
	_, _ = fmt.Fprintln(os.Stderr, "Writing log to", logPath)

	file, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		if closeErr := file.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Error closing log file: %v\n", closeErr)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(file, nil)))
	return cleanup, nil
}

func getTimestamp() string {
	return time.Now().Format("20060102_150405")
}
