package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	chamberCI "github.com/donglin-wang/chamber/internal/ci"
	"github.com/donglin-wang/chamber/pkg/shared/logging"
	"github.com/google/uuid"
)

type statusUpdater interface {
	CreateStatus(context.Context, string, githubStatus) error
}

type ciRunner func(context.Context, chamberCI.Config) (int, error)

type checkoutFunc func(context.Context, string, string, string) error

type server struct {
	cfg          config
	runSlots     chan struct{}
	statusClient statusUpdater
	runCI        ciRunner
	checkout     checkoutFunc
	now          func() time.Time
}

type pushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func newWebhookServer(cfg config) *server {
	return &server{
		cfg:          cfg,
		runSlots:     make(chan struct{}, cfg.MaxParallel),
		statusClient: newGitHubStatusClient(cfg),
		runCI:        chamberCI.Run,
		checkout:     checkoutExactSHA,
		now:          time.Now,
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /github/webhook", s.handleWebhook)
	mux.HandleFunc("GET /runs/{runID}", s.handleRun)
	mux.HandleFunc("GET /runs/{runID}/logs/{job}/{stream}", s.handleLog)
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := ensureRootWritable(s.cfg.Root); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unhealthy",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

func (s *server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	event := r.Header.Get("X-GitHub-Event")
	if event != "push" {
		logging.Info(ctx, "rejected GitHub webhook", "event", event, "reason", "unsupported event")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported GitHub event"})
		return
	}
	deliveryID := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if deliveryID == "" {
		logging.Info(ctx, "rejected GitHub webhook", "event", event, "reason", "missing delivery ID")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing X-GitHub-Delivery"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		logging.Info(ctx, "rejected GitHub webhook", "event", event, "delivery_id", deliveryID, "reason", "read body", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read webhook body"})
		return
	}
	if !validateSignature(s.cfg.GitHubToken, body, r.Header.Get("X-Hub-Signature-256")) {
		logging.Info(ctx, "rejected GitHub webhook", "event", event, "delivery_id", deliveryID, "reason", "invalid signature")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid webhook signature"})
		return
	}

	var payload pushPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		logging.Info(ctx, "rejected GitHub webhook", "event", event, "delivery_id", deliveryID, "content_type", r.Header.Get("Content-Type"), "reason", "invalid payload", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid webhook payload"})
		return
	}
	if payload.Repository.FullName != s.cfg.Repository {
		logging.Info(ctx, "rejected GitHub webhook", "event", event, "delivery_id", deliveryID, "repository", payload.Repository.FullName, "reason", "repository not allowed")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "repository is not allowed"})
		return
	}
	if payload.Deleted {
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":      "ignored",
			"reason":      "deleted ref",
			"delivery_id": deliveryID,
		})
		return
	}
	if !validGitCommitSHA(payload.After) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid after SHA"})
		return
	}
	if !s.tryAcquireRunSlot() {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "all CI slots are busy"})
		return
	}

	runID := uuid.NewString()
	dirs, err := s.createRunDirectories(runID)
	if err != nil {
		s.releaseRunSlot()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create run"})
		return
	}
	record := runRecord{
		RunID:      runID,
		SHA:        payload.After,
		Ref:        payload.Ref,
		Repository: payload.Repository.FullName,
		Status:     runStatusRunning,
		StartedAt:  s.now().UTC(),
		Logs:       map[string]string{},
	}
	if err := writeRunRecord(s.cfg.Root, record); err != nil {
		s.releaseRunSlot()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write run record"})
		return
	}
	go s.runCIForPush(context.Background(), record, dirs)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":      "accepted",
		"run_id":      runID,
		"delivery_id": deliveryID,
	})
}

type runDirectories struct {
	checkout string
	logs     string
}

func (s *server) createRunDirectories(runID string) (runDirectories, error) {
	if !isSafePathComponent(runID) {
		return runDirectories{}, fmt.Errorf("invalid run ID %q", runID)
	}
	runDir := filepath.Join(s.cfg.Root, "runs", runID)
	dirs := runDirectories{
		checkout: filepath.Join(runDir, "checkout"),
		logs:     filepath.Join(runDir, "logs"),
	}
	for _, path := range []string{dirs.checkout, dirs.logs} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return runDirectories{}, fmt.Errorf("create %s: %w", path, err)
		}
	}
	return dirs, nil
}

func (s *server) runCIForPush(parent context.Context, record runRecord, dirs runDirectories) {
	defer s.releaseRunSlot()

	ctx, cancel := context.WithTimeout(parent, s.cfg.RunTimeout)
	defer cancel()
	s.reportGitHubCommitStatus(ctx, record, runStatusRunning)

	gitRemote := "https://github.com/" + s.cfg.Repository + ".git"
	if err := s.checkout(ctx, dirs.checkout, gitRemote, record.SHA); err != nil {
		record.Status = runStatusErrored
		record.Error = err.Error()
		s.completeRun(ctx, record)
		return
	}
	result, err := s.runCI(ctx, chamberCI.Config{
		Root:    filepath.Join(s.cfg.Root, "ci"),
		Workdir: dirs.checkout,
		Image:   chamberCI.DefaultImage,
		Timeout: s.cfg.RunTimeout,
		Keep:    false,
	})
	if err != nil {
		record.Status = runStatusErrored
		record.Error = err.Error()
		record.ExitCode = 1
		s.completeRun(ctx, record)
		return
	}
	record.ExitCode = result
	if result == 0 {
		record.Status = runStatusSucceeded
	} else {
		record.Status = runStatusFailed
	}
	s.completeRun(ctx, record)
}

func (s *server) completeRun(ctx context.Context, record runRecord) {
	completedAt := s.now().UTC()
	record.CompletedAt = &completedAt
	if err := writeRunRecord(s.cfg.Root, record); err != nil {
		logging.Error(ctx, "write CI run record failed", "run_id", record.RunID, "error", err)
	}
	s.reportGitHubCommitStatus(ctx, record, record.Status)
	if s.cfg.Retention > 0 {
		if err := pruneOldRuns(s.cfg.Root, s.now().Add(-s.cfg.Retention)); err != nil {
			logging.Error(ctx, "prune old CI runs failed", "error", err)
		}
	}
}

func (s *server) reportGitHubCommitStatus(ctx context.Context, record runRecord, status runStatus) {
	payload := githubStatusForOutcome(status)
	payload.Context = githubStatusContext
	if s.cfg.StatusTargetBaseURL != "" {
		payload.TargetURL = s.cfg.StatusTargetBaseURL + "/runs/" + record.RunID
	}
	statusCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.statusClient.CreateStatus(statusCtx, record.SHA, payload); err != nil {
		logging.Error(ctx, "GitHub status update failed", "run_id", record.RunID, "sha", record.SHA, "state", payload.State, "error", err)
	}
}

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runID")
	record, err := readRunRecord(s.cfg.Root, runID)
	if err != nil {
		status := http.StatusInternalServerError
		if os.IsNotExist(err) || strings.Contains(err.Error(), "invalid run ID") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": "run not found"})
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (s *server) handleLog(w http.ResponseWriter, r *http.Request) {
	path, err := runLogPath(s.cfg.Root, r.PathValue("runID"), r.PathValue("job"), r.PathValue("stream"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "log not found"})
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		status := http.StatusInternalServerError
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": "log not found"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *server) tryAcquireRunSlot() bool {
	select {
	case s.runSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *server) releaseRunSlot() {
	<-s.runSlots
}

func validGitCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func ensureRootWritable(root string) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(root, ".healthz-*")
	if err != nil {
		return err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func pruneOldRuns(root string, olderThan time.Time) error {
	runsRoot := filepath.Join(root, "runs")
	entries, err := os.ReadDir(runsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isSafePathComponent(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(olderThan) {
			if err := os.RemoveAll(filepath.Join(runsRoot, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
