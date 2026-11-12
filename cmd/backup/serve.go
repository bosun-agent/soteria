// Copyright 2026 - Soteria project. SPDX-License-Identifier: MPL-2.0

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	jobStatusPending   = "pending"
	jobStatusRunning   = "running"
	jobStatusSucceeded = "succeeded"
	jobStatusFailed    = "failed"
)

// defaultArchivePath is where a run writes its artifact when the caller does not
// name a location. It matches the image's BACKUP_ARCHIVE default and is expected
// to be mounted into the container.
const defaultArchivePath = "/archive"

// backupRequest is the JSON body accepted by POST /v1/backups. It describes a
// single backup run that was triggered by an external approval workflow. The
// server never decides *whether* a backup should happen: it only executes the
// trigger it receives and reports a verifiable result.
type backupRequest struct {
	// JobID is an optional caller-supplied correlation id. When empty a random
	// one is generated. Supplying the id of the approval request makes the
	// backup traceable back to the action it protects.
	JobID string `json:"job_id,omitempty"`
	// Volume is an optional label for the volume being backed up. It is only
	// used for reporting and is not interpreted by the server.
	Volume string `json:"volume,omitempty"`
	// Source is the path inside the container that should be archived. The
	// volume has to be mounted into the container before the server starts.
	Source string `json:"source"`
	// Archive is the directory the archive is written to. It doubles as the
	// rollback source, so it has to exist and be writable; when it is left out
	// the run falls back to BACKUP_ARCHIVE, or to defaultArchivePath.
	Archive string `json:"archive,omitempty"`
	// Filename overrides the generated file name. Defaults to
	// job-<job_id>.<extension> so callers can find the artifact deterministically.
	Filename string `json:"filename,omitempty"`
	// RetentionDays overrides BACKUP_RETENTION_DAYS for this run. Use -1 to
	// disable pruning for backups that have to stay available for rollback.
	RetentionDays *int32 `json:"retention_days,omitempty"`
	// Wait makes the request block until the run has finished and return the
	// final result instead of a job id.
	Wait bool `json:"wait,omitempty"`
}

// backupArtifact describes the archive that was verified on disk after a run.
type backupArtifact struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// jobResult is the state of one backup run as reported by GET /v1/backups/{id}.
type jobResult struct {
	JobID      string          `json:"job_id"`
	Volume     string          `json:"volume,omitempty"`
	Status     string          `json:"status"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Error      string          `json:"error,omitempty"`
	Artifact   *backupArtifact `json:"artifact,omitempty"`
}

type jobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*jobResult
}

func newJobRegistry() *jobRegistry {
	return &jobRegistry{jobs: map[string]*jobResult{}}
}

func (r *jobRegistry) put(j *jobResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.JobID] = j
}

func (r *jobRegistry) get(id string) (jobResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return jobResult{}, false
	}
	return *j, true
}

func (r *jobRegistry) exists(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.jobs[id]
	return ok
}

type backupServer struct {
	addr     string
	logger   *slog.Logger
	registry *jobRegistry
}

// configMu serializes configuration construction. envconfig exposes a package
// level lookup function that is swapped while a config is being built, so two
// concurrent requests must not build configs at the same time. The backup run
// itself is serialized separately by the file lock.
var configMu sync.Mutex

func newJobID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// configFromRequest builds a Config for one request without touching the
// process environment. Values not set on the request fall back to the container
// environment so storage credentials keep working as before.
func configFromRequest(req *backupRequest) (*Config, error) {
	filename := req.Filename
	if filename == "" {
		filename = "job-" + req.JobID + ".tar.gz"
	}
	values := map[string]string{
		"BACKUP_SOURCES":  req.Source,
		"BACKUP_ARCHIVE":  req.Archive,
		"BACKUP_FILENAME": filename,
	}
	if req.RetentionDays != nil {
		values["BACKUP_RETENTION_DAYS"] = fmt.Sprintf("%d", *req.RetentionDays)
	}

	lookup := func(key string) (string, bool) {
		if v, ok := values[key]; ok {
			return v, true
		}
		return os.LookupEnv(key)
	}

	configMu.Lock()
	defer configMu.Unlock()

	c, err := loadConfig(lookup)
	if err != nil {
		return nil, err
	}
	c.source = "from http request"
	return c, nil
}

// validArtifactName rejects names that are used as file names but carry a path
// separator or a parent reference. Without this check a caller can move the
// staged archive out of /tmp (filename) or change the artifact name the job id
// is supposed to derive (job_id).
func validArtifactName(field, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if len(name) > 128 {
		return fmt.Errorf("%s %q is longer than 128 characters", field, name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("%s %q must not contain a path separator", field, name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf(`%s %q must not contain ".."`, field, name)
	}
	if name != filepath.Base(name) {
		return fmt.Errorf("%s %q is not a plain file name", field, name)
	}
	return nil
}

func validateRequest(req *backupRequest) error {
	if req.JobID != "" {
		if err := validArtifactName("job_id", req.JobID); err != nil {
			return err
		}
	}
	if req.Filename != "" {
		if err := validArtifactName("filename", req.Filename); err != nil {
			return err
		}
	}
	if strings.TrimSpace(req.Source) == "" {
		return fmt.Errorf("source is required")
	}
	source, err := filepath.Abs(req.Source)
	if err != nil {
		return fmt.Errorf("source %q is not a valid path: %w", req.Source, err)
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("source %q is not readable: %w", source, err)
	}
	if strings.TrimSpace(req.Archive) == "" {
		// A caller should not have to know where the image keeps its artifacts,
		// so an omitted archive is filled in. The directory still has to exist:
		// the artifact on disk is what the gate verifies.
		if fromEnv := strings.TrimSpace(os.Getenv("BACKUP_ARCHIVE")); fromEnv != "" {
			req.Archive = fromEnv
		} else {
			req.Archive = defaultArchivePath
		}
	}
	archive, err := filepath.Abs(req.Archive)
	if err != nil {
		return fmt.Errorf("archive %q is not a valid path: %w", req.Archive, err)
	}
	fi, err := os.Stat(archive)
	if err != nil {
		return fmt.Errorf("archive %q does not exist: %w", archive, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("archive %q is not a directory", archive)
	}
	return nil
}

// verifyArtifact makes sure the run actually left an archive behind. A run that
// archives and immediately discards its output (for example when no storage
// backend is registered) exits successfully without producing anything, so the
// exit code alone is not enough to authorize a destructive action.
func verifyArtifact(c *Config, stats *Stats) (*backupArtifact, error) {
	if stats == nil || stats.BackupFile.Name == "" {
		return nil, fmt.Errorf("backup run produced no artifact metadata")
	}
	full := filepath.Join(c.BackupArchive, stats.BackupFile.Name)
	fi, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("artifact %q is missing after a successful run: %w", full, err)
	}
	if fi.Size() == 0 {
		return nil, fmt.Errorf("artifact %q is empty", full)
	}
	sum, err := fileSHA256(full)
	if err != nil {
		return nil, err
	}
	return &backupArtifact{
		Path:   full,
		Name:   stats.BackupFile.Name,
		Size:   fi.Size(),
		SHA256: sum,
	}, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("error opening artifact for checksum: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("error hashing artifact: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runJob executes one backup run and records its final state.
func (s *backupServer) runJob(id string, req *backupRequest) *jobResult {
	res := &jobResult{
		JobID:     id,
		Volume:    req.Volume,
		Status:    jobStatusRunning,
		StartedAt: time.Now(),
	}
	s.registry.put(res)

	fail := func(err error) *jobResult {
		now := time.Now()
		res.Status = jobStatusFailed
		res.Error = err.Error()
		res.FinishedAt = &now
		res.DurationMS = now.Sub(res.StartedAt).Milliseconds()
		s.registry.put(res)
		s.logger.Error("backup run failed", "job_id", id, "error", err)
		return res
	}

	c, err := configFromRequest(req)
	if err != nil {
		return fail(fmt.Errorf("error building configuration: %w", err))
	}

	var stats *Stats
	if err := runScript(c, runScriptOpts{onStats: func(st *Stats) { stats = st }}); err != nil {
		return fail(err)
	}

	artifact, err := verifyArtifact(c, stats)
	if err != nil {
		return fail(err)
	}

	now := time.Now()
	res.Status = jobStatusSucceeded
	res.Artifact = artifact
	res.FinishedAt = &now
	res.DurationMS = now.Sub(res.StartedAt).Milliseconds()
	s.registry.put(res)
	s.logger.Info(
		"backup run succeeded",
		"job_id", id,
		"artifact", artifact.Path,
		"size", artifact.Size,
		"sha256", artifact.SHA256,
	)
	return res
}

func (s *backupServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// The API reads JSON only. Demanding the media type also keeps a random web
	// page from triggering a run from the user's browser: a cross-origin request
	// carrying this header is preceded by a preflight that the server never
	// allows, while a `text/plain` body would be sent without one.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{
			"error": "Content-Type must be application/json",
		})
		return
	}

	var req backupRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}

	if err := validateRequest(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if req.JobID == "" {
		req.JobID = newJobID()
	}
	if s.registry.exists(req.JobID) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":  "job_id already exists",
			"job_id": req.JobID,
		})
		return
	}

	if req.Wait {
		done := make(chan *jobResult, 1)
		go func() { done <- s.runJob(req.JobID, &req) }()
		select {
		case res := <-done:
			code := http.StatusOK
			if res.Status != jobStatusSucceeded {
				code = http.StatusInternalServerError
			}
			writeJSON(w, code, res)
		case <-r.Context().Done():
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{
				"error":  "request cancelled while waiting for the backup to finish; poll GET /v1/backups/{job_id}",
				"job_id": req.JobID,
			})
		}
		return
	}

	s.registry.put(&jobResult{
		JobID:     req.JobID,
		Volume:    req.Volume,
		Status:    jobStatusPending,
		StartedAt: time.Now(),
	})
	go func() { s.runJob(req.JobID, &req) }()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"job_id": req.JobID,
		"status": jobStatusPending,
	})
}

func (s *backupServer) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/backups/")
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid job id"})
		return
	}
	res, ok := s.registry.get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found", "job_id": id})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *backupServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("error encoding response", "error", err)
	}
}

// runServe starts the approval-triggered backup API and blocks until the
// process receives a signal. It never schedules runs on its own: every backup
// is the result of an explicit external trigger.
func runServe(addr string) error {
	s := &backupServer{
		addr:     addr,
		logger:   slog.New(slog.NewTextHandler(os.Stdout, nil)),
		registry: newJobRegistry(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/backups", s.handleCreate)
	mux.HandleFunc("/v1/backups/", s.handleGet)
	mux.HandleFunc("/healthz", s.handleHealth)

	s.logger.Info("starting approval-triggered backup API", "addr", addr)

	// The API authenticates nobody, so anybody who can reach the address can
	// start a run and read artifact paths and digests. Binding every interface
	// is the right default inside a container (a published port cannot reach a
	// socket that only listens on the container's loopback), but it has to be a
	// conscious choice.
	if host, _, err := net.SplitHostPort(addr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		s.logger.Warn(
			"the API accepts connections from every host that can reach this address and requires no authentication; publish it on 127.0.0.1 instead (for example -p 127.0.0.1:8080:8080), or pass -http-addr 127.0.0.1:<port> when running the binary directly",
			"addr", addr,
		)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("error serving http: %w", err)
	}
	return nil
}
