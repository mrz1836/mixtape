// Package session tracks recording sessions across processes: the global
// recorder lock, the live session.json state document, the per-session
// NDJSON event log, detaching into the background, and the Spotify
// settings a session changed and must put back.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrBusy reports that another recorder holds the lock.
var ErrBusy = errors.New("another mixtape recording session is running")

// LockInfo describes the process holding the recorder lock. It is written
// into the lock file so status, wait and stop can find the session even
// when it was started with a different --home.
type LockInfo struct {
	PID       int       `json:"pid"`
	SessionID string    `json:"session_id"`
	Home      string    `json:"home"`
	StartedAt time.Time `json:"started_at"`
}

// DefaultLockPath is the per-user recorder lock. It deliberately ignores
// --home: one Mac has one Spotify, so two homes must not record at once.
func DefaultLockPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mixtape", "recorder.lock"), nil
}

// Lock is a held recorder lock.
type Lock struct {
	f *os.File
}

// Acquire takes the recorder lock at path without blocking and records
// info in it. When another process holds it, the error wraps ErrBusy and
// names the holder.
func Acquire(path string, info LockInfo) (*Lock, error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
		holder, _ := readLockInfo(f)
		_ = f.Close()
		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (session %s, pid %d)", ErrBusy, holder.SessionID, holder.PID)
		}
		return nil, fmt.Errorf("lock %s: %w", path, lockErr)
	}
	data, err := json.Marshal(info)
	if err == nil {
		err = f.Truncate(0)
	}
	if err == nil {
		_, err = f.WriteAt(data, 0)
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release clears the holder record and drops the lock. It is idempotent.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = l.f.Truncate(0)
	// Unlock explicitly: a child forked meanwhile can briefly share the
	// open file description, and closing ours alone would not release it.
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	return err
}

// Inspect reports whether the recorder lock at path is held and, if so, by
// whom. It never blocks and never leaves the lock taken.
func Inspect(path string) (LockInfo, bool, error) {
	f, err := openLockFile(path)
	if err != nil {
		return LockInfo{}, false, err
	}
	defer func() { _ = f.Close() }()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			info, readErr := readLockInfo(f)
			return info, true, readErr
		}
		return LockInfo{}, false, fmt.Errorf("inspect lock %s: %w", path, err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return LockInfo{}, false, nil
}

func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Clean(path), os.O_RDWR|os.O_CREATE, 0o600)
}

func readLockInfo(f *os.File) (LockInfo, error) {
	var info LockInfo
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return info, err
	}
	buf := make([]byte, min(st.Size(), 4096))
	n, err := f.ReadAt(buf, 0)
	if n == 0 && err != nil {
		return info, err
	}
	if err := json.Unmarshal(buf[:n], &info); err != nil {
		return LockInfo{}, fmt.Errorf("read lock holder: %w", err)
	}
	return info, nil
}
