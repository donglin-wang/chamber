package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasscodePersistsAndCorruptionDoesNotRegenerate(t *testing.T) {
	root := t.TempDir()
	first, err := updatePasscode(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := updatePasscode(root)
	if err != nil || first != second || len(first) != 64 {
		t.Fatalf("passcode changed: %v", err)
	}
	path := filepath.Join(root, "update.passcode")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := updatePasscode(root); err == nil {
		t.Fatal("accepted corrupt passcode")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "broken" {
		t.Fatal("replaced existing passcode")
	}
}

func TestBoundedLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	l, err := openLog(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := l.Write([]byte("abcdefg")); n != 7 || err != nil {
		t.Fatalf("write: %d %v", n, err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = openLog(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = l.Write([]byte("more"))
	_ = l.Close()
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, []byte("abcde")) {
		t.Fatalf("cap: %q", data)
	}
}

func TestConfigDefaultsAndRejection(t *testing.T) {
	t.Setenv("CI_TEST_TOKEN", "token")
	t.Setenv("CI_TEST_SECRET", "secret")
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	valid := `{"root":` + `"` + root + `","public_url":"https://ci.example.test","repositories":{"test":{"name":"owner/repo","token_env":"CI_TEST_TOKEN","secret_env":"CI_TEST_SECRET"}}}`
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxParallel != 1 || c.Repositories["test"].CloneURL != "https://github.com/owner/repo.git" || c.jobTimeout <= 0 {
		t.Fatalf("defaults: %+v", c)
	}
	for _, bad := range []string{
		strings.Replace(valid, `"public_url":`, `"unknown":true,"public_url":`, 1),
		strings.Replace(valid, "https://ci.example.test", "https://ci.example.test/path", 1),
		strings.Replace(valid, "CI_TEST_TOKEN", "MISSING_TOKEN", 1),
		valid + ` {}`,
	} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatal("bad config accepted")
		}
	}
}

func TestMissingPasscodeIsNeverReplaced(t *testing.T) {
	root := t.TempDir()
	if _, err := updatePasscode(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "update.passcode")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := updatePasscode(root); err == nil {
		t.Fatal("replaced a lost passcode")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("created a replacement", err)
	}
	for _, name := range []string{"jobs.sqlite"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := updatePasscode(root); err == nil {
			t.Fatalf("ignored existing %s", name)
		}
	}
}
