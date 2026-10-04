// Package durable writes files so that a crash or power loss leaves either the
// old contents or the new contents on disk, never a truncated file.
//
// Auth files hold single-use refresh tokens. Writing one in place (open with
// O_TRUNC, then write) leaves a window where the only copy of a rotated token
// is a zero-length or half-written file. WriteFile closes that window.
package durable

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Seams for tests. Production code never reassigns them.
var (
	writeData = func(f *os.File, data []byte) error {
		_, err := f.Write(data)
		return err
	}
	syncFile   = func(f *os.File) error { return f.Sync() }
	renameFile = os.Rename
	syncDir    = syncDirectory
)

// WriteFile replaces path with data. It writes a temp file in the same
// directory, fsyncs it, renames it over path and fsyncs the directory, so the
// rename itself survives a crash. On any error before the rename, path keeps
// its old contents and the temp file is removed.
//
// When path is a symlink, the link target is replaced and the link is kept.
// The parent directory must already exist.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	target, err := resolveTarget(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return fmt.Errorf("durable write %s: create temp: %w", target, err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err = tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return fmt.Errorf("durable write %s: chmod temp: %w", target, err)
	}
	if err = writeData(tmp, data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("durable write %s: write temp: %w", target, err)
	}
	if err = syncFile(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("durable write %s: fsync temp: %w", target, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("durable write %s: close temp: %w", target, err)
	}
	if err = renameFile(tmpPath, target); err != nil {
		return fmt.Errorf("durable write %s: rename: %w", target, err)
	}
	renamed = true
	if err = syncDir(dir); err != nil {
		return fmt.Errorf("durable write %s: fsync dir: %w", target, err)
	}
	return nil
}

// resolveTarget follows a symlink at path so the rename replaces the file the
// link points to, as an in-place write did, instead of the link itself.
func resolveTarget(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("durable write %s: resolve symlink: %w", path, err)
	}
	return target, nil
}

// syncDirectory fsyncs dir so a rename into it is on disk. Windows cannot open
// a directory for sync, and NTFS journals the rename, so it is a no-op there.
func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err = d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// WriteJSON encodes v the way json.Encoder does (HTML escaped, trailing
// newline), with indent when it is not empty, and writes it with WriteFile.
func WriteJSON(path string, v any, indent string, perm os.FileMode) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("durable write %s: encode: %w", path, err)
	}
	return WriteFile(path, buf.Bytes(), perm)
}
