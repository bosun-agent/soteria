// Copyright 2026 - Soteria project. SPDX-License-Identifier: MPL-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRequest(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "data")
	archive := filepath.Join(dir, "archive")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		req     backupRequest
		wantErr bool
	}{
		{
			name:    "missing source",
			req:     backupRequest{Archive: archive},
			wantErr: true,
		},
		{
			name:    "source does not exist",
			req:     backupRequest{Source: filepath.Join(dir, "nope"), Archive: archive},
			wantErr: true,
		},
		{
			name:    "archive missing",
			req:     backupRequest{Source: source},
			wantErr: true,
		},
		{
			name:    "archive does not exist",
			req:     backupRequest{Source: source, Archive: filepath.Join(dir, "nope")},
			wantErr: true,
		},
		{
			name: "valid",
			req:  backupRequest{Source: source, Archive: archive},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRequest(&tc.req)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestConfigFromRequestUsesRequestValues(t *testing.T) {
	req := &backupRequest{
		JobID:   "approval-1",
		Source:  "/backup/target",
		Archive: "/archive",
	}
	c, err := configFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BackupSources != "/backup/target" {
		t.Errorf("BackupSources = %q, want %q", c.BackupSources, "/backup/target")
	}
	if c.BackupArchive != "/archive" {
		t.Errorf("BackupArchive = %q, want %q", c.BackupArchive, "/archive")
	}
	// The file name has to be derivable from the job id so callers can locate
	// the artifact without parsing logs.
	if c.BackupFilename != "job-approval-1.tar.gz" {
		t.Errorf("BackupFilename = %q, want %q", c.BackupFilename, "job-approval-1.tar.gz")
	}
	if c.source != "from http request" {
		t.Errorf("source = %q, want %q", c.source, "from http request")
	}
}

func TestConfigFromRequestHonorsOverrides(t *testing.T) {
	retention := int32(-1)
	req := &backupRequest{
		JobID:         "approval-2",
		Source:        "/backup/target",
		Archive:       "/archive",
		Filename:      "custom.tar.gz",
		RetentionDays: &retention,
	}
	c, err := configFromRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BackupFilename != "custom.tar.gz" {
		t.Errorf("BackupFilename = %q, want %q", c.BackupFilename, "custom.tar.gz")
	}
	if c.BackupRetentionDays != -1 {
		t.Errorf("BackupRetentionDays = %d, want -1", c.BackupRetentionDays)
	}
}

func TestVerifyArtifactFailsWhenNothingWasStored(t *testing.T) {
	archive := t.TempDir()
	c := &Config{BackupArchive: archive}
	stats := &Stats{BackupFile: BackupFileStats{Name: "job-x.tar.gz"}}

	if _, err := verifyArtifact(c, stats); err == nil {
		t.Fatal("expected an error for a missing artifact, got none")
	}
}

func TestVerifyArtifactReportsChecksum(t *testing.T) {
	archive := t.TempDir()
	name := "job-x.tar.gz"
	content := []byte("archive-bytes")
	if err := os.WriteFile(filepath.Join(archive, name), content, 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Config{BackupArchive: archive}
	stats := &Stats{BackupFile: BackupFileStats{Name: name}}

	artifact, err := verifyArtifact(c, stats)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if artifact.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", artifact.Size, len(content))
	}
	sum := sha256.Sum256(content)
	if artifact.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256 = %q, want %q", artifact.SHA256, hex.EncodeToString(sum[:]))
	}
	if artifact.Path != filepath.Join(archive, name) {
		t.Errorf("Path = %q, want %q", artifact.Path, filepath.Join(archive, name))
	}
}

func TestVerifyArtifactFailsWhenEmpty(t *testing.T) {
	archive := t.TempDir()
	name := "empty.tar.gz"
	if err := os.WriteFile(filepath.Join(archive, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Config{BackupArchive: archive}
	stats := &Stats{BackupFile: BackupFileStats{Name: name}}

	if _, err := verifyArtifact(c, stats); err == nil {
		t.Fatal("expected an error for an empty artifact, got none")
	}
}

func TestJobRegistryRejectsDuplicateInHandler(t *testing.T) {
	r := newJobRegistry()
	r.put(&jobResult{JobID: "a", Status: jobStatusPending})
	if !r.exists("a") {
		t.Fatal("expected job a to exist")
	}
	if r.exists("b") {
		t.Fatal("did not expect job b to exist")
	}
	got, ok := r.get("a")
	if !ok || got.Status != jobStatusPending {
		t.Fatalf("unexpected registry result: %+v ok=%v", got, ok)
	}
}

// The caller-supplied filename is used as the staged file name, and the job id
// ends up in the generated file name. Both have to stay inside the directories
// the server owns.
func TestValidateRequestRejectsNamesThatEscapeTheArchive(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "data")
	archive := filepath.Join(dir, "archive")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		filename string
		jobID    string
		wantErr  bool
	}{
		{name: "plain filename", filename: "custom.tar.gz"},
		{name: "plain job id", jobID: "approval-42"},
		{name: "filename with a parent reference", filename: "../x.tar.gz", wantErr: true},
		{name: "filename with a separator", filename: "nested/x.tar.gz", wantErr: true},
		{name: "filename that is a bare parent reference", filename: "..", wantErr: true},
		{name: "job id with a parent reference", jobID: "../../escaped", wantErr: true},
		{name: "job id with a backslash", jobID: `evil\name`, wantErr: true},
		{name: "job id longer than 128 characters", jobID: strings.Repeat("a", 129), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := backupRequest{Source: source, Archive: archive, Filename: tc.filename, JobID: tc.jobID}
			err := validateRequest(&req)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestHandleCreateRequiresJSONContentType(t *testing.T) {
	s := &backupServer{
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		registry: newJobRegistry(),
	}

	// A body sent as text/plain would be delivered by a browser without a
	// preflight, so it must not start a run.
	req := httptest.NewRequest(http.MethodPost, "/v1/backups", strings.NewReader(`{"source":"/backup/target"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	s.handleCreate(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusUnsupportedMediaType, rec.Body.String())
	}

	// With the JSON media type the request reaches validation instead. The
	// source does not exist here, so the run is refused with 400.
	req = httptest.NewRequest(http.MethodPost, "/v1/backups",
		strings.NewReader(`{"source":"/backup/target","archive":"`+t.TempDir()+`"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec = httptest.NewRecorder()
	s.handleCreate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}
