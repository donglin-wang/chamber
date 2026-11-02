package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type server struct {
	config           config
	store            jobStore
	ctx              context.Context
	passcode         string
	client           *http.Client
	mu               sync.Mutex // protects admission and update state, not job counts
	accepting        bool
	updating         bool
	draining         bool
	processing       sync.WaitGroup
	processingCancel context.CancelFunc
	shutdownTimeout  time.Duration
	candidate        string
	updateReady      chan struct{}
	tasks            sync.WaitGroup // waits for cleanup; it does not limit parallel jobs
	execute          func(context.Context, config, job, io.Writer) (string, error)
	cleanup          func(context.Context, config, job) error
	buildUpdate      func(context.Context) (string, error)
}

func newServer(c config, passcode string, store jobStore) (*server, error) {
	if err := validateStoredJobs(context.Background(), c, store); err != nil {
		return nil, err
	}
	if err := store.RecoverInterrupted(context.Background()); err != nil {
		return nil, err
	}
	return &server{
		config: c, store: store, ctx: context.Background(), passcode: passcode, accepting: true,
		shutdownTimeout: 45 * time.Second, updateReady: make(chan struct{}), execute: executeJob, cleanup: cleanupJob,
		buildUpdate: func(ctx context.Context) (string, error) { return buildUpdate(ctx, c) },
		client:      &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func validateStoredJobs(ctx context.Context, c config, store jobStore) error {
	jobs, err := store.Unsettled(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if !j.Cleaned && j.Workspace != jobWorkspace(c, j.ID) {
			return fmt.Errorf("job %s still owns workspace %s; restore root/work_root before recovery", j.ID, j.Workspace)
		}
		repo, ok := c.Repositories[j.Repository]
		if !ok || !strings.EqualFold(repo.Name, j.RepositoryName) {
			return fmt.Errorf("repository %s still has unreconciled job %s; restore its config first", j.Repository, j.ID)
		}
	}
	return nil
}

var errShutdownTimeout = errors.New("CI shutdown deadline exceeded")

// The caller retains the listening socket while this HTTP server shuts down.
func (s *server) serve(shutdown context.Context, listener net.Listener) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	processingCtx, processingCancel := context.WithCancel(ctx)
	s.processingCancel = processingCancel
	httpServer := &http.Server{Handler: routes(s), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10}
	serveError := make(chan error, 1)
	s.processing.Add(2)
	go func() { defer s.processing.Done(); s.dispatchJobs(processingCtx) }()
	go func() { defer s.processing.Done(); s.retryStatusUpdates(processingCtx) }()
	s.tasks.Add(1)
	go func() { defer s.tasks.Done(); serveError <- httpServer.Serve(listener) }()
	slog.Info("CI listening", "address", listener.Addr(), "revision", revision, "update_passcode_file", filepath.Join(s.config.Root, "update.passcode"))
	var err error
	updating := false
	select {
	case <-shutdown.Done():
	case <-s.updateReady:
		updating = shutdown.Err() == nil
	case err = <-serveError:
	}
	stopCtx, done := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer done()
	err = errors.Join(err, s.stop(stopCtx, httpServer, cancel, updating))
	if err != nil || !updating {
		return "", err
	}
	return s.candidate, nil
}

func waitForCleanup(ctx context.Context, group *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errShutdownTimeout
	}
}

func (s *server) stop(ctx context.Context, httpServer *http.Server, cancel context.CancelFunc, updating bool) error {
	s.mu.Lock()
	if !updating {
		s.accepting = false
	} // During update, finish admitting HTTP requests already in flight.
	cancel()
	s.mu.Unlock()
	if err := httpServer.Shutdown(ctx); err != nil {
		_ = httpServer.Close()
	}
	if err := waitForCleanup(ctx, &s.processing); err != nil {
		return err
	}
	if err := waitForCleanup(ctx, &s.tasks); err != nil {
		return err
	}
	if updating {
		return nil // Queued jobs transfer to the replacement.
	}
	os.Remove(s.candidate)
	if err := s.store.RecoverInterrupted(ctx); err != nil {
		return err
	}
	if err := s.store.CancelQueued(ctx); err != nil {
		return err
	}
	return s.reportJobStatuses(ctx) // One bounded final attempt; failures remain durable.
}

func routes(s *server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, revision) })
	mux.HandleFunc("POST /webhooks/{repository}", s.postWebhook)
	mux.HandleFunc("GET /runs/{id}", s.getRun)
	mux.HandleFunc("GET /runs/{id}/log", s.getRunLog)
	mux.HandleFunc("POST /update", s.postUpdate)
	return mux
}

func (s *server) postWebhook(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("repository")
	repo, ok := s.config.Repositories[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		http.Error(w, "webhook too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	if !verifySignature(body, r.Header.Get("X-Hub-Signature-256"), repo.secret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	sha, ref, err := parseEvent(r.Header.Get("X-GitHub-Event"), body, repo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sha == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := jobID(key, sha)
	j := job{ID: id, Repository: key, RepositoryName: repo.Name, Workspace: jobWorkspace(s.config, id), SHA: sha, Ref: ref, State: "queued", Message: "Waiting for CI worker", CreatedAt: time.Now().UTC()}
	s.mu.Lock()
	if !s.accepting {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "10")
		http.Error(w, "CI server is draining", http.StatusServiceUnavailable)
		return
	}
	saved, created, err := s.store.Admit(r.Context(), j, s.config.MaxQueued)
	s.mu.Unlock()
	if errors.Is(err, errQueueFull) {
		w.Header().Set("Retry-After", "10")
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	if err != nil {
		slog.Error("admit job", "error", err)
		http.Error(w, "cannot persist job", http.StatusInternalServerError)
		return
	}
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", s.config.PublicURL+"/runs/"+id)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(saved)
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, errJobNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "cannot read job", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = runTemplate.Execute(w, j)
}

func (s *server) getRunLog(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, errJobNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "cannot read job", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	f, err := os.Open(filepath.Join(s.config.Root, "logs", j.ID+".log"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		http.Error(w, "cannot read log", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	_, _ = io.Copy(w, io.LimitReader(f, s.config.MaxLogBytes))
}

func (s *server) postUpdate(w http.ResponseWriter, r *http.Request) {
	authorization := r.Header.Get("Authorization")
	provided := strings.TrimPrefix(authorization, "Bearer ")
	if !strings.HasPrefix(authorization, "Bearer ") || subtle.ConstantTimeCompare([]byte(provided), []byte(s.passcode)) != 1 {
		http.Error(w, "invalid update passcode", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	if s.updating || !s.accepting {
		s.mu.Unlock()
		http.Error(w, "update or shutdown already in progress", http.StatusConflict)
		return
	}
	s.updating = true
	s.tasks.Add(1)
	s.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, "Update accepted; progress is in update.log\n")
	go func() {
		defer s.tasks.Done()
		if err := s.prepareUpdate(s.ctx); err != nil {
			if s.ctx.Err() == nil {
				slog.Error("prepare update", "error", err)
			}
			s.mu.Lock()
			os.Remove(s.candidate)
			s.candidate = ""
			s.updating = false
			s.accepting = s.ctx.Err() == nil
			s.draining = false
			s.mu.Unlock()
			return
		}
		close(s.updateReady)
	}()
}

func (s *server) prepareUpdate(ctx context.Context) error {
	buildCtx, cancel := context.WithTimeout(ctx, s.config.jobTimeout)
	candidate, err := s.buildUpdate(buildCtx)
	cancel()
	if err != nil {
		os.Remove(candidate)
		return err
	}
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		os.Remove(candidate)
		return ctx.Err()
	}
	s.candidate = candidate
	s.draining = true
	s.mu.Unlock()
	// HTTP admission continues. Queued rows transfer to the replacement.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		count, err := s.store.RunningCount(ctx)
		if err != nil {
			return err
		}
		if count == 0 {
			if s.processingCancel != nil {
				s.processingCancel()
				if err := waitForCleanup(ctx, &s.processing); err != nil {
					return err
				}
			}
			// Stop old status requests before the replacement starts its reporter.
			flushCtx, done := context.WithTimeout(ctx, 15*time.Second)
			defer done()
			if err := s.reportJobStatuses(flushCtx); err != nil && ctx.Err() == nil {
				slog.Warn("handoff status delivery remains pending", "error", err)
			}
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Goroutines execute jobs; the store's atomic Claim operation decides how many
// may run. The WaitGroup below only waits for their cleanup during shutdown.
func (s *server) dispatchJobs(ctx context.Context) {
	var executions sync.WaitGroup
	defer executions.Wait()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.mu.Lock()
		var j *job
		var err error
		if !s.draining {
			j, err = s.store.Claim(ctx, s.config.MaxParallel)
		}
		s.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			slog.Error("claim job; will retry", "error", err)
		}
		if err == nil && j != nil {
			executions.Add(1)
			go func(j job) { defer executions.Done(); s.runJob(ctx, j) }(*j)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *server) runJob(parent context.Context, j job) {
	ctx, cancel := context.WithTimeout(parent, s.config.jobTimeout)
	defer cancel()
	state, message := "error", "CI worker did not complete"
	var output *boundedLog
	defer func() {
		if recovered := recover(); recovered != nil {
			state, message = "error", "CI worker panicked"
			slog.Error("CI worker panic", "id", j.ID, "panic", recovered)
		}
		if ctx.Err() != nil {
			state, message = "error", "CI job canceled or timed out"
		}
		if output != nil {
			fmt.Fprintln(output, message)
			if err := output.Close(); err != nil {
				state, message = "error", "Could not persist CI log"
				slog.Error(message, "error", err)
			}
		}
		// A slot remains occupied until the terminal result is durable. Retrying a
		// failed write cannot accidentally let the dispatcher exceed max_parallel.
		for {
			saveCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.store.Finish(saveCtx, j.ID, state, message)
			done()
			if err == nil {
				return
			}
			slog.Error("save job outcome; slot remains occupied", "id", j.ID, "error", err)
			select {
			case <-parent.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	var err error
	output, err = openLog(filepath.Join(s.config.Root, "logs", j.ID+".log"), s.config.MaxLogBytes)
	if err != nil {
		slog.Error("open job log", "error", err)
		return
	}
	if ctx.Err() != nil {
		return
	}
	state, err = s.execute(ctx, s.config, j, output)
	if err != nil {
		fmt.Fprintln(output, err)
		message = "Dockerfile.chamber CI failed; see logs"
	} else {
		message = "Dockerfile.chamber CI passed"
	}
}

func (s *server) retryStatusUpdates(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.reportJobUpdates(ctx); err != nil && ctx.Err() == nil {
			slog.Error("report jobs; will retry", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *server) reportJobStatuses(ctx context.Context) error {
	jobs, err := s.store.Unsettled(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.Reported {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := reportStatus(ctx, s.client, s.config.Repositories[j.Repository], j, s.config.PublicURL); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("GitHub status; will retry", "id", j.ID, "error", err)
			continue
		}
		if err := s.store.Acknowledge(ctx, j.ID, j.Version); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) reportJobUpdates(ctx context.Context) error {
	if err := s.reportJobStatuses(ctx); err != nil {
		return err
	}
	jobs, err := s.store.Unsettled(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if !j.terminal() || j.Cleaned {
			continue
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := s.cleanup(cleanupCtx, s.config, j)
		cancel()
		if err != nil {
			slog.Error("cleanup job; will retry", "id", j.ID, "error", err)
		} else if err := s.store.MarkCleaned(ctx, j.ID); err != nil {
			return err
		}
	}
	return nil
}

var runTemplate = template.Must(template.New("run").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Chamber CI</title>
<style>body{max-width:1000px;margin:2rem auto;padding:0 1rem;font:16px system-ui;background:#10151b;color:#e3edf5}pre{white-space:pre-wrap;overflow-wrap:anywhere;padding:1rem;background:#18212c}a{color:#8ed0ff}</style>
<h1>Chamber CI: {{.State}}</h1><p>{{.Message}}</p><p>{{.SHA}}</p><p><a href="{{.ID}}/log">Raw log</a></p><pre id="log">Loading log…</pre>
<script>async function refresh(){try{const r=await fetch(location.pathname+'/log',{cache:'no-store'});if(r.ok)document.getElementById('log').textContent=await r.text()}catch(e){} }refresh();setInterval(refresh,2000)</script></html>`))
