package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type runLogger struct {
	mu   sync.Mutex
	file *os.File
	path string
}

func newRunLogger(clusterName string) (*runLogger, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory for run log: %w", err)
	}
	component := safeLogComponent(clusterName)
	if component == "" {
		return nil, fmt.Errorf("invalid cluster name for run log")
	}
	dir := filepath.Join(home, ".searke", "logs", component)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create run log directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure run log directory: %w", err)
	}
	name := "apply-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".log"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create run log: %w", err)
	}
	pruneRunLogs(dir, 10)
	return &runLogger{file: f, path: path}, nil
}

// pruneRunLogs keeps the newest `keep` logs; names sort chronologically.
func pruneRunLogs(dir string, keep int) {
	matches, err := filepath.Glob(filepath.Join(dir, "apply-*.log"))
	if err != nil || len(matches) <= keep {
		return
	}
	sort.Strings(matches)
	for _, p := range matches[:len(matches)-keep] {
		os.Remove(p)
	}
}

func safeLogComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), ".")
}

func (l *runLogger) Log(format string, args ...any) {
	line := time.Now().Format(time.RFC3339) + " " + fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = os.Stderr.WriteString(line)
	_, _ = l.file.WriteString(line)
}

func (l *runLogger) Path() string { return l.path }

func (l *runLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Sync()
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
