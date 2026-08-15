package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestValidateSignature(t *testing.T) {
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	secret := "webhook-secret"
	signature := signedBody(secret, body)

	if !validateSignature(secret, body, signature) {
		t.Fatal("validateSignature() = false, want true")
	}
	if validateSignature(secret, body, "sha256=bad") {
		t.Fatal("validateSignature() accepted mismatched signature")
	}
	if validateSignature(secret, body, "") {
		t.Fatal("validateSignature() accepted missing signature")
	}
	if validateSignature(secret, body, "sha1=bad") {
		t.Fatal("validateSignature() accepted wrong algorithm")
	}
}

func TestGitHubStatusForOutcome(t *testing.T) {
	tests := []struct {
		name        string
		status      runStatus
		wantState   statusState
		description string
	}{
		{name: "running", status: runStatusRunning, wantState: statusPending, description: "Chamber CI is running on OCI A1 ARM64"},
		{name: "succeeded", status: runStatusSucceeded, wantState: statusSuccess, description: "Chamber CI passed"},
		{name: "failed", status: runStatusFailed, wantState: statusFailure, description: "Chamber CI failed"},
		{name: "errored", status: runStatusErrored, wantState: statusError, description: "Chamber CI errored before tests completed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := githubStatusForOutcome(test.status)
			if status.State != test.wantState {
				t.Fatalf("State = %q, want %q", status.State, test.wantState)
			}
			if status.Description != test.description {
				t.Fatalf("Description = %q, want %q", status.Description, test.description)
			}
		})
	}
}

func signedBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
