---
title: MCP server
layout: default
parent: How Tos
nav_order: 21
---

# MCP server

`cmd/mcp` exposes the approval-triggered backup API as a
[Model Context Protocol](https://modelcontextprotocol.io) server, so an agent
runtime can request a backup and receive a verified artifact without shelling out.

The server holds no backup logic. It forwards to `backup serve`, which means the
gate is defined in exactly one place and both entrypoints behave identically.

## Build

```console
$ go build -o mcp-soteria ./cmd/mcp
```

## Configure the client

The server speaks MCP over stdio and reads the API location from `SOTERIA_API`
(default `http://127.0.0.1:8080`):

```json
{
  "mcpServers": {
    "soteria-backup": {
      "command": "/usr/local/bin/mcp-soteria",
      "env": {
        "SOTERIA_API": "http://127.0.0.1:8080"
      }
    }
  }
}
```

The backup API has to be reachable — see
[Approval-triggered backups](./approval-triggered-backup.html) for running it.

## Tools

| Tool | Purpose |
| --- | --- |
| `backup_volume` | Start a backup run and return the verified artifact (path, size, sha256). |
| `backup_status` | Look up a run by `job_id` and report its status and artifact. |

`backup_volume` arguments:

| Argument | Required | Description |
| --- | --- | --- |
| `source` | yes | Path inside the backup container to archive. |
| `archive` | no | Directory the archive is written to. Defaults to `/archive` on the API side. |
| `job_id` | no | Correlation id. Pass the approval id so the backup is traceable. |
| `volume` | no | Label for reporting. |
| `wait` | no | Block until the run finishes and return the artifact. `false` returns a job id immediately. |

Example result:

```json
{
  "job_id": "approval-42",
  "status": "succeeded",
  "duration_ms": 10,
  "artifact": {
    "path": "/archive/job-approval-42.tar.gz",
    "name": "job-approval-42.tar.gz",
    "size": 239,
    "sha256": "8657b6cb0f9288cdfa1314875a41b8db645ca0dca850ae2d30f2bbb31ea59e68"
  }
}
```

## Failure behaviour

The tool reports `isError: true` and does not present the run as clean when:

- the API rejects the request (`400` for an unreadable source or a missing archive);
- the `job_id` already exists (`409`);
- the API is unreachable;
- the run reports `succeeded` but carries **no digest**. This last case is
  deliberate: a success without a verified artifact is treated as a failure, so a
  broken API cannot hand a caller a clean-looking result.

## What this does not enforce

MCP lets an agent *call* the backup. It does not force the agent to call it before
destroying data — a model that ignores the instruction can still invoke some other
tool first.

If ordering has to be guaranteed rather than requested, do not expose backup and
destruction as separate tools. Expose a single tool that performs
`stop → backup → verify → destroy` internally, and give the agent no other way to
reach the destructive step. That gate has to hold the Docker socket, which this
server deliberately does not.

## Tests

```console
$ go test ./cmd/mcp/
```

The tests cover the success path, the missing-digest case, `400`/`409`/`404`
handling and an unreachable API, using a local `httptest` server.