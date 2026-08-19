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

func signedBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
