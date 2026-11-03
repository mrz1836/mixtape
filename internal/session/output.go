package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/fileutil"
)

// outputRestoreName is the record of an output rate to put back.
const outputRestoreName = "output-restore.json"

// OutputRestorePath is where a session records the output device rate it
// switched and must put back: next to the recorder lock (see
// DefaultLockPath), not in a --home, because one Mac has one output and the
// record must outlive the session that wrote it. session.json would not
// do: a recorder rewrites it from memory.
func OutputRestorePath(lockPath string) string {
	return filepath.Join(filepath.Dir(lockPath), outputRestoreName)
}

// ReadOutputRestore reads the record at path; no file is no record.
func ReadOutputRestore(path string) (capture.OutputRestore, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return capture.OutputRestore{}, nil
	}
	if err != nil {
		return capture.OutputRestore{}, fmt.Errorf("read the output rate to restore: %w", err)
	}
	var r capture.OutputRestore
	if err := json.Unmarshal(data, &r); err != nil {
		return capture.OutputRestore{}, fmt.Errorf("read the output rate to restore: %s: %w", path, err)
	}
	return r, nil
}

// WriteOutputRestore saves r at path, atomically; a record with nothing
// pending removes the file.
func WriteOutputRestore(path string, r capture.OutputRestore) error {
	if !r.Pending() {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear the output rate to restore: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save the output rate to restore: %w", err)
	}
	if err := fileutil.WriteJSON(path, r, 0o600); err != nil {
		return fmt.Errorf("save the output rate to restore: %w", err)
	}
	return nil
}
