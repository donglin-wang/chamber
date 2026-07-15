package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageRemoveRequiresYes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "xdg-data")
	err := runStorage([]string{"remove"}, mapGetenv(map[string]string{
		"XDG_DATA_HOME": root,
	}), &bytes.Buffer{})
	if err == nil {
		t.Fatal("runStorage() error = nil, want confirmation error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("runStorage() error = %v, want --yes hint", err)
	}
}

func TestStorageRemoveDeletesDerivedChamberRoot(t *testing.T) {
	xdgDataHome := filepath.Join(t.TempDir(), "xdg-data")
	chamberRoot := filepath.Join(xdgDataHome, "chamber")
	if err := os.MkdirAll(filepath.Join(chamberRoot, "images"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(chamberRoot, "images", "index.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var output bytes.Buffer
	err := runStorage([]string{"remove", "--yes"}, mapGetenv(map[string]string{
		"XDG_DATA_HOME": xdgDataHome,
		"HOME":          filepath.Join(t.TempDir(), "home"),
	}), &output)
	if err != nil {
		t.Fatalf("runStorage() error = %v", err)
	}

	if _, err := os.Stat(chamberRoot); !os.IsNotExist(err) {
		t.Fatalf("storage root stat error = %v, want not exist", err)
	}
	if !strings.Contains(output.String(), chamberRoot) {
		t.Fatalf("output = %q, want removed path", output.String())
	}
}

func TestStorageRemoveMissingRootSucceeds(t *testing.T) {
	xdgDataHome := filepath.Join(t.TempDir(), "xdg-data")
	chamberRoot := filepath.Join(xdgDataHome, "chamber")

	var output bytes.Buffer
	err := runStorage([]string{"remove", "--yes"}, mapGetenv(map[string]string{
		"XDG_DATA_HOME": xdgDataHome,
	}), &output)
	if err != nil {
		t.Fatalf("runStorage() error = %v", err)
	}

	if !strings.Contains(output.String(), chamberRoot) {
		t.Fatalf("output = %q, want missing path", output.String())
	}
}

func TestValidateStorageRootRejectsNonChamberPath(t *testing.T) {
	err := validateStorageRoot(filepath.Join(t.TempDir(), "not-chamber"))
	if err == nil {
		t.Fatal("validateStorageRoot() error = nil, want error")
	}
}

func mapGetenv(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}
