// Package fsutil holds crash-safe file helpers.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes via temp file, fsync, rename and directory fsync.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := MkdirAllDurable(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(dir)
}

// MkdirAllDurable is os.MkdirAll that also fsyncs the parent of every
// directory it creates, so the new entries survive a power loss.
func MkdirAllDurable(dir string, perm os.FileMode) error {
	dir = filepath.Clean(dir)
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return &os.PathError{Op: "mkdir", Path: dir, Err: os.ErrExist}
		}
		return nil
	}
	if err := MkdirAllDurable(filepath.Dir(dir), perm); err != nil {
		return err
	}
	if err := os.Mkdir(dir, perm); err != nil && !os.IsExist(err) {
		return err
	}
	return SyncDir(filepath.Dir(dir))
}

// SyncDir fsyncs a directory (after creating or renaming entries in it).
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
