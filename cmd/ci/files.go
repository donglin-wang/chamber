package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Sync the file and its directory before returning success.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// The caller holds the root lock. The marker distinguishes a first startup
// from a damaged installation whose passcode must never be silently replaced.
func updatePasscode(root string) (string, error) {
	path := filepath.Join(root, "update.passcode")
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		for _, name := range []string{"initialized", "jobs.sqlite"} {
			if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
				return "", fmt.Errorf("existing CI installation has no update.passcode; restore the original file")
			}
		}
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return "", err
		}
		if err := writeAtomic(path, []byte(hex.EncodeToString(secret)), 0600); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	passcode, err := readPasscode(root)
	if err != nil {
		return "", err
	}
	marker := filepath.Join(root, "initialized")
	if _, err := os.Lstat(marker); os.IsNotExist(err) {
		if err := writeAtomic(marker, []byte("1\n"), 0600); err != nil {
			return "", err
		}
	} else {
		data, err := os.ReadFile(marker)
		if err != nil {
			return "", err
		}
		if string(data) != "1\n" {
			return "", fmt.Errorf("invalid CI initialization marker")
		}
	}
	return passcode, nil
}

func readPasscode(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "update.passcode"))
	if err != nil {
		return "", err
	}
	decoded, err := hex.DecodeString(string(data))
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("invalid update.passcode; refusing to replace it")
	}
	return string(data), nil
}

type boundedLog struct {
	mu        sync.Mutex
	f         *os.File
	remaining int64
}

func openLog(path string, limit int64) (*boundedLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &boundedLog{f: f, remaining: max(0, limit-info.Size())}, nil
}

func (l *boundedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	if _, err := l.f.Write(p); err != nil {
		return 0, err
	}
	l.remaining -= int64(len(p))
	return n, nil
}

func (l *boundedLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return errors.Join(l.f.Sync(), l.f.Close())
}

// Keep the log writer's contract explicit at its asynchronous runtime boundary.
var _ io.WriteCloser = (*boundedLog)(nil)
