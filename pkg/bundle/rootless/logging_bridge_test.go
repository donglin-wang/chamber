package rootless

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	apexlog "github.com/apex/log"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

func TestApexBridgeRedirectsToSharedLogger(t *testing.T) {
	var buffer bytes.Buffer
	old := chamberLogging.Logger()
	chamberLogging.SetLogger(chamberLogging.NewJSONLogger(&buffer, slog.LevelInfo))
	t.Cleanup(func() {
		chamberLogging.SetLogger(old)
	})
	installApexBridge()

	apexlog.WithField("digest", "sha256:abc").Info("unpack layer")

	entry := decodeLogEntry(t, buffer.Bytes())
	if entry["msg"] != "unpack layer" {
		t.Fatalf("msg = %v, want unpack layer", entry["msg"])
	}
	if entry["library"] != "umoci" {
		t.Fatalf("library = %v, want umoci", entry["library"])
	}
	if entry["digest"] != "sha256:abc" {
		t.Fatalf("digest = %v, want sha256:abc", entry["digest"])
	}
}

func TestApexBridgeDebugLogsAreFilteredBySharedLogger(t *testing.T) {
	var buffer bytes.Buffer
	old := chamberLogging.Logger()
	chamberLogging.SetLogger(chamberLogging.NewJSONLogger(&buffer, slog.LevelInfo))
	t.Cleanup(func() {
		chamberLogging.SetLogger(old)
	})
	installApexBridge()

	apexlog.Debug("debug detail")

	if buffer.Len() != 0 {
		t.Fatalf("debug log produced output: %s", buffer.String())
	}
}

func decodeLogEntry(t *testing.T, data []byte) map[string]any {
	t.Helper()

	var entry map[string]any
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("decode log entry %q: %v", string(data), err)
	}
	return entry
}
