package main

import "testing"

func TestCanonicalImageOperationLockKeySerializesEquivalentReferences(t *testing.T) {
	shortKey, shortReference, err := canonicalImageOperationLockKey("alpine")
	if err != nil {
		t.Fatalf("canonicalImageOperationLockKey(alpine) error = %v", err)
	}
	fullKey, fullReference, err := canonicalImageOperationLockKey("docker.io/library/alpine:latest")
	if err != nil {
		t.Fatalf("canonicalImageOperationLockKey(full) error = %v", err)
	}
	if shortKey != fullKey || shortReference != fullReference {
		t.Fatalf("equivalent image locks = (%q, %q) and (%q, %q), want identical", shortKey, shortReference, fullKey, fullReference)
	}
}
