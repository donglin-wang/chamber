package runc

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	chruntime "github.com/donglin-wang/chamber/internal/runtime"
	"github.com/donglin-wang/chamber/internal/shared/localfs"
)

func TestEnsureDownloadsValidRuntimeBinary(t *testing.T) {
	content := []byte("valid runc")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	binDir := privateTempDir(t)
	runtime := New(chruntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(content),
	}, localfs.NewDirectoryManager())

	binary, err := runtime.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	if binary.Name != "runc" {
		t.Fatalf("Binary.Name = %q, want runc", binary.Name)
	}
	if binary.Version != "test-version" {
		t.Fatalf("Binary.Version = %q, want test-version", binary.Version)
	}
	if binary.Path != filepath.Join(binDir, "runc") {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, filepath.Join(binDir, "runc"))
	}
	assertFileContentAndMode(t, binary.Path, content, 0755)
}

func TestEnsureRejectsWrongDigest(t *testing.T) {
	content := []byte("not the pinned binary")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	binDir := privateTempDir(t)
	runtime := New(chruntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex([]byte("expected binary")),
	}, localfs.NewDirectoryManager())

	_, err := runtime.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() error = nil, want digest error")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Ensure() error = %v, want checksum failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "runc")); !os.IsNotExist(statErr) {
		t.Fatalf("final binary stat error = %v, want not exist", statErr)
	}
}

func TestEnsureRejectsNonOKResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	runtime := New(chruntime.Config{
		RuntimeBinDir: privateTempDir(t),
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex([]byte("anything")),
	}, localfs.NewDirectoryManager())

	_, err := runtime.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() error = nil, want HTTP status error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("Ensure() error = %v, want HTTP 404", err)
	}
}

func TestEnsureRejectsInterruptedBody(t *testing.T) {
	content := []byte("partial")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("Hijack() error = %v", err)
		}
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(content)+10, content)
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)

	binDir := privateTempDir(t)
	runtime := New(chruntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(content),
	}, localfs.NewDirectoryManager())

	_, err := runtime.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() error = nil, want interrupted body error")
	}
	if _, statErr := os.Stat(filepath.Join(binDir, "runc")); !os.IsNotExist(statErr) {
		t.Fatalf("final binary stat error = %v, want not exist", statErr)
	}
}

func TestEnsureUsesExistingValidBinary(t *testing.T) {
	content := []byte("already cached")
	binDir := privateTempDir(t)
	path := filepath.Join(binDir, "runc")
	if err := os.WriteFile(path, content, 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	t.Cleanup(server.Close)

	runtime := New(chruntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(content),
	}, localfs.NewDirectoryManager())

	binary, err := runtime.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if binary.Path != path {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, path)
	}
	if requests != 0 {
		t.Fatalf("download requests = %d, want 0", requests)
	}
	assertFileContentAndMode(t, path, content, 0755)
}

func TestEnsureReplacesExistingInvalidBinary(t *testing.T) {
	oldContent := []byte("corrupt cached binary")
	newContent := []byte("replacement binary")

	binDir := privateTempDir(t)
	path := filepath.Join(binDir, "runc")
	if err := os.WriteFile(path, oldContent, 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(newContent)
	}))
	t.Cleanup(server.Close)

	runtime := New(chruntime.Config{
		RuntimeBinDir: binDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(newContent),
	}, localfs.NewDirectoryManager())

	binary, err := runtime.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if binary.Path != path {
		t.Fatalf("Binary.Path = %q, want %q", binary.Path, path)
	}
	assertFileContentAndMode(t, path, newContent, 0755)
}

func TestEnsureReturnsAbsolutePath(t *testing.T) {
	content := []byte("absolute")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	relativeBinDir := filepath.Join(".", t.Name())
	t.Cleanup(func() {
		_ = os.RemoveAll(relativeBinDir)
	})

	runtime := New(chruntime.Config{
		RuntimeBinDir: relativeBinDir,
		Name:          "runc",
		Version:       "test-version",
		URL:           server.URL,
		SHA256:        sha256Hex(content),
	}, localfs.NewDirectoryManager())

	binary, err := runtime.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if !filepath.IsAbs(binary.Path) {
		t.Fatalf("Binary.Path = %q, want absolute path", binary.Path)
	}
}

func TestEnsureRequiresCompleteConfiguration(t *testing.T) {
	runtime := New(chruntime.Config{
		RuntimeBinDir: privateTempDir(t),
		Name:          "runc",
		Version:       "test-version",
		URL:           "http://example.test/runc",
	}, localfs.NewDirectoryManager())

	_, err := runtime.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure() error = nil, want configuration error")
	}
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum[:])
}

func privateTempDir(t *testing.T) string {
	t.Helper()

	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
	return path
}

func assertFileContentAndMode(t *testing.T, path string, wantContent []byte, wantMode os.FileMode) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != string(wantContent) {
		t.Fatalf("content at %q = %q, want %q", path, content, wantContent)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if info.Mode().Perm() != wantMode {
		t.Fatalf("mode at %q = %o, want %o", path, info.Mode().Perm(), wantMode)
	}
}
