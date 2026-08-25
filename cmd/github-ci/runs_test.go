package main

import (
	"testing"
)

func TestRunLogPathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		runID  string
		job    string
		stream string
	}{
		{runID: "..", job: "pkg", stream: "stdout"},
		{runID: "00000000-0000-0000-0000-000000000000", job: "../pkg", stream: "stdout"},
		{runID: "00000000-0000-0000-0000-000000000000", job: "pkg", stream: "../stdout"},
	} {
		if _, err := runLogPath(root, test.runID, test.job, test.stream); err == nil {
			t.Fatalf("runLogPath(%q, %q, %q) error = nil, want rejection", test.runID, test.job, test.stream)
		}
	}
}

func TestRunProofLogPathRejectsEscapesAndNonTextEvidence(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		runID string
		path  string
	}{
		{runID: "..", path: "daemon-supervised/stdout.log"},
		{runID: "00000000-0000-0000-0000-000000000000", path: "../stdout.log"},
		{runID: "00000000-0000-0000-0000-000000000000", path: "daemon-lifecycle/../../stdout.log"},
		{runID: "00000000-0000-0000-0000-000000000000", path: "daemon-lifecycle/chamberd"},
	} {
		if _, err := runProofLogPath(root, test.runID, test.path); err == nil {
			t.Fatalf("runProofLogPath(%q, %q) error = nil, want rejection", test.runID, test.path)
		}
	}
}
