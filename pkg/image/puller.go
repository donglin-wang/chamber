package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
)

var ErrRootRequired = errors.New("image root is required")

type PullRequest struct {
	Reference string
	Platform  Platform
	Auth      *Auth
}

type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

type Auth struct {
	Username string
	Password string
	Token    string
}

type PulledImage struct {
	Reference  string
	Digest     string
	LayoutPath string
	SizeBytes  int64
	PulledAt   time.Time
}

type Puller interface {
	Pull(ctx context.Context, request PullRequest) (PulledImage, error)
}

func Destination(root string, reference string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", ErrRootRequired
	}
	sum := sha256.Sum256([]byte(reference))
	return filepath.Join(root, hex.EncodeToString(sum[:])), nil
}

func LayoutExists(path string) bool {
	return ValidateLayout(path) == nil
}

func ValidateLayout(path string) error {
	if strings.TrimSpace(path) == "" {
		return ErrRootRequired
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("OCI image layout path is not a directory")
	}

	layoutFile, err := os.ReadFile(filepath.Join(path, "oci-layout"))
	if err != nil {
		return err
	}
	var layoutVersion struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(layoutFile, &layoutVersion); err != nil {
		return err
	}
	if layoutVersion.ImageLayoutVersion == "" {
		return errors.New("OCI image layout version is missing")
	}

	indexFile, err := os.ReadFile(filepath.Join(path, "index.json"))
	if err != nil {
		return err
	}
	var index imagespec.Index
	if err := json.Unmarshal(indexFile, &index); err != nil {
		return err
	}
	if len(index.Manifests) == 0 {
		return errors.New("OCI image layout index has no manifests")
	}
	return nil
}
