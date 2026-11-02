// Package fileutil provides crash-safe filesystem helpers: every write lands
// in a temporary file in the destination directory, is fsynced, and is then
// renamed into place, so a reader sees either the old file or the new one,
// never a torn mix.
package fileutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrEmptyPath indicates an empty file path was provided.
var ErrEmptyPath = errors.New("path is empty")

// TempPrefix starts the name of every temporary file this package creates.
// Cleanup code matches on it to remove leftovers from a crashed write.
const TempPrefix = ".mixtape-tmp-"

// WriteAtomic writes data to path atomically with the provided permissions.
// It writes to a temp file in the same directory, fsyncs, then renames.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	return writeAtomic(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// CopyFile copies src to dst atomically: the bytes stream into a temp file
// next to dst, which is fsynced and renamed over dst. It works across
// filesystems (a library on an external drive), unlike os.Rename.
func CopyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(filepath.Clean(src))
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	return writeAtomic(dst, perm, func(w io.Writer) error {
		_, copyErr := io.Copy(w, in)
		return copyErr
	})
}

// IsTemp reports whether name (a base name) is a temp file left by this
// package.
func IsTemp(name string) bool { return strings.HasPrefix(name, TempPrefix) }

func writeAtomic(path string, perm os.FileMode, fill func(io.Writer) error) error {
	if path == "" {
		return ErrEmptyPath
	}

	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	tmpPath := tmpFile.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmpFile.Close()
		}
		_ = os.Remove(tmpPath) // no-op once renamed
	}()

	if err := fill(tmpFile); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmpFile.Chmod(perm); err != nil {
		return fmt.Errorf("setting temp file permissions: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	closed = true

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}

	// Best-effort directory sync so the rename itself is durable.
	if dirFile, err := os.Open(filepath.Clean(dir)); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

// WriteJSON writes v to path atomically (see WriteAtomic) as indented JSON
// ending in a newline. The folder must exist.
func WriteJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	return WriteAtomic(path, append(data, '\n'), perm)
}

// OpenAppend opens path for appending, creating the file (mode 0600) and
// its folder (mode 0700) as needed: a log only its owner may read.
func OpenAppend(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
}
