// Copyright 2026 - Soteria project. SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const successBody = `{
  "job_id": "approval-1",
  "volume": "data",
  "status": "succeeded",
  "duration_ms": 3,
  "artifact": {
    "path": "/archive/job-approval-1.tar.gz",
    "name": "job-approval-1.tar.gz",
    "size": 232,
    "sha256": "bb30a8b586f7515cb189c50380f9ead54a369c9032466330912dcf5d39de5ae2"
  }
}`

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestBackupVolumeReportsArtifact(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successBody))
	}))
	defer srv.Close()

	res, _, err := newAPIClient(srv.URL).backupVolume(context.Background(), nil, backupArgs{
		Source:  "/backup/target",
		Archive: "/archive",
		Wait:    true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/backups" {
		t.Fatalf("called %s %s, want POST /v1/backups", gotMethod, gotPath)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", textOf(t, res))
	}
	text := textOf(t, res)
	if !strings.Contains(text, "bb30a8b586f7515cb189c50380f9ead54a369c9032466330912dcf5d39de5ae2") {
		t.Fatalf("response does not carry the digest: %s", text)
	}
}

func TestBackupVolumeRejectsSuccessWithoutDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"job_id":"x","status":"succeeded"}`))
	}))
	defer srv.Close()

	res, _, err := newAPIClient(srv.URL).backupVolume(context.Background(), nil, backupArgs{Source: "/backup/target"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("a success without a digest must be reported as an error")
	}
}

func TestBackupVolumeRejectsBadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"source \"/nope\" is not readable"}`))
	}))
	defer srv.Close()

	res, _, err := newAPIClient(srv.URL).backupVolume(context.Background(), nil, backupArgs{Source: "/nope"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("a 400 must be reported as an error")
	}
	if !strings.Contains(textOf(t, res), "not readable") {
		t.Fatalf("underlying message missing: %s", textOf(t, res))
	}
}

func TestBackupVolumeReportsDuplicateJobID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"job_id already exists","job_id":"a"}`))
	}))
	defer srv.Close()

	res, _, _ := newAPIClient(srv.URL).backupVolume(context.Background(), nil, backupArgs{
		Source: "/backup/target",
		JobID:  "a",
	})
	if !res.IsError {
		t.Fatal("a 409 must be reported as an error")
	}
	if !strings.Contains(textOf(t, res), "already exists") {
		t.Fatalf("message does not mention the conflict: %s", textOf(t, res))
	}
}

func TestBackupStatusReturnsJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/backups/approval-1" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(successBody))
	}))
	defer srv.Close()

	res, _, err := newAPIClient(srv.URL).backupStatus(context.Background(), nil, statusArgs{JobID: "approval-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got error: %s", textOf(t, res))
	}
	if !strings.Contains(textOf(t, res), "/archive/job-approval-1.tar.gz") {
		t.Fatalf("artifact path missing: %s", textOf(t, res))
	}
}

func TestBackupStatusUnknownJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"job not found"}`))
	}))
	defer srv.Close()

	res, _, _ := newAPIClient(srv.URL).backupStatus(context.Background(), nil, statusArgs{JobID: "nope"})
	if !res.IsError {
		t.Fatal("a 404 must be reported as an error")
	}
}

func TestBackupStatusRequiresJobID(t *testing.T) {
	res, _, _ := newAPIClient("http://127.0.0.1:1").backupStatus(context.Background(), nil, statusArgs{})
	if !res.IsError {
		t.Fatal("a missing job_id must be reported as an error")
	}
}

func TestUnreachableAPIIsAnError(t *testing.T) {
	res, _, _ := newAPIClient("http://127.0.0.1:1").backupVolume(context.Background(), nil, backupArgs{
		Source: "/backup/target",
	})
	if !res.IsError {
		t.Fatal("an unreachable API must be reported as an error")
	}
}

func TestServerRegistersBothTools(t *testing.T) {
	s := newServer(newAPIClient("http://127.0.0.1:1"))
	if s == nil {
		t.Fatal("server is nil")
	}
}
