// Copyright 2026 - Soteria project. SPDX-License-Identifier: MPL-2.0

// Command mcp exposes the approval-triggered backup API as a Model Context
// Protocol server. It does not contain backup logic of its own: it forwards to
// the HTTP API served by `backup serve`, so both entrypoints share one gate.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// apiClient talks to the backup API. It is a struct so tests can point it at a
// httptest server instead of a real container.
type apiClient struct {
	base string
	hc   *http.Client
}

func newAPIClient(base string) *apiClient {
	return &apiClient{
		base: base,
		// A backup can legitimately take a long time; the HTTP timeout must not
		// be shorter than the largest volume you expect to archive.
		hc: &http.Client{Timeout: 30 * time.Minute},
	}
}

func (c *apiClient) call(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("backup API unreachable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("unexpected response (%d): %s", resp.StatusCode, string(raw))
	}
	return resp.StatusCode, nil
}

// artifactFile is the nested payload the API returns once an archive has been
// verified on disk.
type artifactFile struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// backupResult is the API's job view. Only Status == "succeeded" together with
// a non-empty Artifact.SHA256 means an archive actually exists and was hashed.
type backupResult struct {
	JobID      string        `json:"job_id"`
	Volume     string        `json:"volume,omitempty"`
	Status     string        `json:"status"`
	Error      string        `json:"error,omitempty"`
	DurationMS int64         `json:"duration_ms,omitempty"`
	Artifact   *artifactFile `json:"artifact,omitempty"`
}

type backupArgs struct {
	Volume  string `json:"volume,omitempty" jsonschema:"label for the volume being backed up, reporting only"`
	Source  string `json:"source" jsonschema:"path inside the backup container to archive, for example /backup/target"`
	Archive string `json:"archive,omitempty" jsonschema:"directory the archive is written to, defaults to /archive"`
	JobID   string `json:"job_id,omitempty" jsonschema:"correlation id, use the id of the approval this backup protects"`
	Wait    bool   `json:"wait,omitempty" jsonschema:"block until the run finishes and return the verified artifact; false returns a job id immediately"`
}

type statusArgs struct {
	JobID string `json:"job_id" jsonschema:"the job id returned by backup_volume"`
}

func boolPtr(v bool) *bool { return &v }

func textResult(v any) *mcp.CallToolResult {
	payload, _ := json.MarshalIndent(v, "", "  ")
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}},
	}
}

func errorResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// successResult refuses to present a success without a verified digest as
// clean. A gate must never treat "succeeded" alone as clearance.
func successResult(r backupResult) *mcp.CallToolResult {
	if r.Status == "succeeded" && (r.Artifact == nil || r.Artifact.SHA256 == "") {
		return errorResult("backup reported success but returned no verified artifact digest; treat this as a failure")
	}
	return textResult(r)
}

func (c *apiClient) backupVolume(ctx context.Context, req *mcp.CallToolRequest, args backupArgs) (*mcp.CallToolResult, any, error) {
	body := map[string]any{
		"source": args.Source,
		"wait":   args.Wait,
	}
	if args.Volume != "" {
		body["volume"] = args.Volume
	}
	if args.Archive != "" {
		body["archive"] = args.Archive
	}
	if args.JobID != "" {
		body["job_id"] = args.JobID
	}

	var out backupResult
	code, err := c.call(ctx, http.MethodPost, "/v1/backups", body, &out)
	if err != nil {
		return errorResult("backup request failed: %v", err), nil, nil
	}
	switch code {
	case http.StatusOK, http.StatusAccepted:
		return successResult(out), nil, nil
	case http.StatusConflict:
		return errorResult("job_id %q already exists; do not retry blindly", args.JobID), nil, nil
	default:
		if out.Error != "" {
			return errorResult("backup rejected (%d): %s", code, out.Error), nil, nil
		}
		return errorResult("backup rejected with status %d", code), nil, nil
	}
}

func (c *apiClient) backupStatus(ctx context.Context, req *mcp.CallToolRequest, args statusArgs) (*mcp.CallToolResult, any, error) {
	if args.JobID == "" {
		return errorResult("job_id is required"), nil, nil
	}
	var out backupResult
	code, err := c.call(ctx, http.MethodGet, "/v1/backups/"+args.JobID, nil, &out)
	if err != nil {
		return errorResult("status request failed: %v", err), nil, nil
	}
	if code == http.StatusNotFound {
		return errorResult("no backup job named %q", args.JobID), nil, nil
	}
	if code != http.StatusOK {
		return errorResult("status request returned %d", code), nil, nil
	}
	return successResult(out), nil, nil
}

func newServer(c *apiClient) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "soteria-backup",
		Version: "0.1.0",
	}, &mcp.ServerOptions{
		Instructions: "Backup gate for destructive container operations. Call backup_volume " +
			"before removing or replacing anything that owns persistent data, and only proceed " +
			"when the result reports status=succeeded with a sha256.",
	})
	mcp.AddTool(s, &mcp.Tool{
		Name: "backup_volume",
		Description: "Create a backup of a Docker volume and return the verified artifact " +
			"(path, size, sha256). Use this before any destructive action on the volume's data. " +
			"Set wait=true to block until the archive exists on disk; otherwise poll with backup_status.",
		Annotations: &mcp.ToolAnnotations{
			Title: "Backup volume",
			// The run writes an archive but does not remove or overwrite data, so
			// it is additive. Runtimes that gate on annotations see "not
			// destructive" and still have to be granted the tool explicitly in
			// restricted mode, because the hint alone is not a permission.
			ReadOnlyHint:    false,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  false,
			OpenWorldHint:   boolPtr(false),
		},
	}, c.backupVolume)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "backup_status",
		Description: "Look up a backup run started by backup_volume and report its status and verified artifact.",
		Annotations: &mcp.ToolAnnotations{
			Title:        "Backup status",
			ReadOnlyHint: true,
		},
	}, c.backupStatus)
	return s
}

func main() {
	logger := log.New(os.Stderr, "mcp-soteria ", log.LstdFlags)

	base := os.Getenv("SOTERIA_API")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}

	logger.Printf("serving on stdio, api=%s", base)
	if err := newServer(newAPIClient(base)).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		logger.Fatalf("server failed: %v", err)
	}
}
