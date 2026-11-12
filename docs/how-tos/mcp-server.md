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
(default `http://127.0.0.1:18081`, the port the API is published on; the API
itself listens on 8080 inside its container):

```json
{
  "mcpServers": {
    "soteria-backup": {
      "command": "/usr/local/bin/mcp-soteria",
      "env": {
        "SOTERIA_API": "http://127.0.0.1:18081"
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
| `archive` | no | Directory the archive is written to. Defaults to `BACKUP_ARCHIVE` on the API side, or to `/archive`. It has to exist there already. |
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

## Consistency

These tools archive the volume as it is on disk, so a database that keeps writing
while a run is in progress leaves files from different points in time behind: the
call can report `succeeded` and the artifact still be unusable for the rollback it
was taken for. Stop the container (or dump the database) first, and treat the
order

```
stop -> backup_volume -> verify sha256 -> destroy -> start
```

as part of the workflow rather than as advice. The API holds no Docker socket,
reports no field that describes the application state, and exposes no tool that
could stop a container, so this ordering cannot be enforced from here - it has to
live in the agent's instructions.

## Failure behaviour

The tool reports `isError: true` and does not present the run as clean when:

- the API rejects the request (`400` for an unreadable source, an archive that
  does not exist, a `filename` or `job_id` carrying a path separator or `..`, or a
  `retention_days` outside `1..BACKUP_MAX_RETENTION_DAYS`);
- the request was not sent as JSON (`415`);
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

## Registering with docker-agent

docker-agent attaches local stdio servers with `type: mcp` (`command` + `args` + `env`).
Build the binary first, then point the toolset at it:

```console
$ go build -o /usr/local/bin/mcp-soteria ./cmd/mcp
```

```yaml
agents:
  root:
    model: openai/gpt-5-mini
    description: Docker 관리 에이전트
    instruction: |
      Before changing or removing containers, state a short plan and wait for
      confirmation. For a volume that belongs to a database, stop the container
      (or run the dump) before calling backup_volume, verify the returned
      sha256, and only then perform the destructive action. Start the container
      again afterwards. Answer in Korean.
    toolsets:
      - type: mcp
        command: /usr/local/bin/mcp-soteria
        env:
          SOTERIA_API: "http://127.0.0.1:8080"
        instruction: Use backup_volume before any destructive volume operation.

permissions:
  allow:
    # backup_volume is a write: docker-agent labels a tool with no
    # destructive_hint and no read_only_hint as "unknown", and the default
    # restricted mode denies unknown tools outright. Grant it explicitly.
    - backup_volume
```

Two things decide whether the tools are usable at all:

| Tool | Annotations | docker-agent label | Needed |
| --- | --- | --- | --- |
| `backup_status` | `readOnlyHint: true` | safe | allowed by default |
| `backup_volume` | `destructiveHint: false`, `readOnlyHint: false` | **unknown** | must be in `permissions.allow` |

Adding a `name` to the toolset changes the exposed tool names to `<name>_<tool>`;
without it the raw names are used, so the permission entries above match as-is.

`SOTERIA_API` has to be reachable **from the process running the MCP server**:
`127.0.0.1:18081` when docker-agent runs on the host, and
`http://host.docker.internal:18081` (plus `extra_hosts: [host.docker.internal:host-gateway]`)
when it runs in a container with the backup API on the host.

## Tests

```console
$ go test ./cmd/mcp/
```

The tests cover the success path, the missing-digest case, `400`/`409`/`404`
handling and an unreachable API, using a local `httptest` server.