package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/mrz1836/mixtape/internal/fileutil"
)

// EventLog appends one compact JSON document per line to a session's
// NDJSON log. It is safe for concurrent use.
type EventLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// OpenEventLog creates (or appends to) the event log at path.
func OpenEventLog(path string) (*EventLog, error) {
	f, err := fileutil.OpenAppend(path)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	return &EventLog{f: f, path: path}, nil
}

// Path is the log file.
func (l *EventLog) Path() string { return l.path }

// Write appends v as one JSON line.
func (l *EventLog) Write(v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return os.ErrClosed
	}
	_, err := l.f.Write(buf.Bytes())
	return err
}

// Close flushes and closes the log. It is idempotent.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Follow copies complete lines of the log at path to w, starting at byte
// offset from. Each time it reaches the end of the file it calls wait,
// which should pause briefly and report whether to keep following. It
// returns the offset reached, so a caller can resume.
func Follow(path string, from int64, w io.Writer, wait func() bool) (int64, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return from, fmt.Errorf("follow the event log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return from, fmt.Errorf("follow the event log: %w", err)
	}
	r := bufio.NewReader(f)
	offset := from
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if _, werr := w.Write(line); werr != nil {
				return offset, werr
			}
			offset += int64(len(line))
			continue
		}
		// A partial line is re-read once it is complete.
		if len(line) > 0 {
			if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
				return offset, serr
			}
			r.Reset(f)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return offset, err
		}
		if !wait() {
			return offset, nil
		}
	}
}
