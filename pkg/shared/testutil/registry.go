// Package testutil provides helpers for Chamber package tests.
package testutil

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	localregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// FakeRegistry is an in-process OCI registry for tests.
type FakeRegistry struct {
	server *httptest.Server
}

// NewFakeRegistry starts a registry that accepts image pushes and pulls.
func NewFakeRegistry(t testing.TB) *FakeRegistry {
	t.Helper()

	registry := localregistry.New(localregistry.Logger(log.New(io.Discard, "", 0)))
	return newFakeRegistry(t, registry)
}

// NewFailingRegistry starts a registry that returns 404 for every request.
func NewFailingRegistry(t testing.TB) *FakeRegistry {
	t.Helper()

	return newFakeRegistry(t, http.NotFoundHandler())
}

func newFakeRegistry(t testing.TB, handler http.Handler) *FakeRegistry {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &FakeRegistry{
		server: server,
	}
}

// Reference returns an image reference hosted by the fake registry.
func (r *FakeRegistry) Reference(t testing.TB, repo, tag string) string {
	t.Helper()

	u, err := url.Parse(r.server.URL)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", r.server.URL, err)
	}
	return fmt.Sprintf("%s/%s:%s", u.Host, repo, tag)
}

// PushRandomImage pushes a small random image and returns its reference and
// digest.
func (r *FakeRegistry) PushRandomImage(t testing.TB, repo, tag string) (string, v1.Hash) {
	t.Helper()

	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("random.Image() error = %v", err)
	}

	reference := r.Reference(t, repo, tag)
	ref, err := name.ParseReference(reference)
	if err != nil {
		t.Fatalf("ParseReference() error = %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("remote.Write() error = %v", err)
	}

	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	return reference, digest
}
