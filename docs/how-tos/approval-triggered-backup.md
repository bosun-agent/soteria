---
title: Approval-triggered backups
layout: default
parent: How Tos
nav_order: 20
---

# Approval-triggered backups

This fork adds a `serve` command that turns the image into an HTTP endpoint for
**externally triggered** backup runs. It is meant to sit behind an approval
workflow: when a human approves a destructive action, the workflow first asks
this service for a backup, waits for a verified artifact, and only then performs
the action it was approved for.

The service never schedules anything on its own. Without an incoming request it
does nothing at all, so it can safely replace the built-in scheduler in setups
that only need on-demand backups.

## Why a separate trigger path

The upstream `backup` command already runs exactly one backup and exits, which
is the right primitive for an approval gate. What it does not do is report a
machine-readable result: the artifact name only appears in log output, and a run
that archives and discards its own output still exits `0`. A gate that authorises
a destructive action cannot be built on top of that.

`serve` closes both gaps:

- every run is identified by a caller-supplied `job_id` and produces a JSON
  result that includes the artifact path, its size and its SHA-256 checksum;
- a run only reports success once the artifact was found on disk, so an empty
  or discarded archive fails the request instead of passing it.

## Running the service

```yaml
services:
  backup-api:
    image: ghcr.io/<owner>/soteria:latest
    # The upstream image sets an ENTRYPOINT, so override it to run the API
    # instead of the built-in scheduler.
    entrypoint: ["/usr/bin/backup"]
    # Inside the container the API keeps the default address. It has to listen on
    # every interface there, otherwise the published port cannot reach it.
    command: ["serve", "-http-addr", ":8080"]
    restart: unless-stopped
    environment:
      # Setting the timezone keeps log timestamps readable and makes the
      # deprecated timezone bind-mount warnings disappear.
      TZ: Asia/Seoul
      # A gate must not block for an hour when another run is in flight.
      LOCK_TIMEOUT: 10s
      # Upper bound for the retention a request may ask for (default 365).
      BACKUP_MAX_RETENTION_DAYS: 90
    ports:
      # Publish on loopback only: the API authenticates nobody, so every host
      # that can reach the port can start a run. 18081 stays out of the way of
      # docker-agent's own `serve api`, which binds 8080 on the host.
      - "127.0.0.1:18081:8080"
    volumes:
      # The volume to protect has to be mounted before the service starts.
      - app_data:/backup/target:ro
      - /srv/backups:/archive
```

Note that no Docker socket is mounted. Stopping and restarting the containers
that use the volume is the responsibility of the caller: it already owns the
destructive action and knows when the application has to be quiesced. Treat that
as a required step, not as an option.

## Consistency: stop the application before archiving it

**If the volume belongs to an application with a database, stop the container or
dump the database before requesting the backup.** The run archives the files as
they are on disk. A database that keeps writing while the archive is created
produces a set of files that belong to different points in time, and restoring
that gives a database which may not open, or which has lost or duplicated
transactions. The artifact then passes the gate while being unusable for exactly
the rollback it was taken for.

The API cannot do this for you: it does not hold the Docker socket, so it cannot
stop anything, it reports no field that says whether the application was stopped,
and the MCP tools it exposes cannot stop a container either. A workflow that must
not depend on discipline should:

1. stop the container (or run the dump command),
2. call `backup_volume` and wait for the result,
3. verify `artifact.sha256` against the file on disk,
4. only then perform the destructive action and start the container again.

Because a container can only archive paths that are mounted into it, a single
instance covers a fixed set of volumes. To protect volumes that are discovered
at runtime, either mount all of them or run one short-lived container per target
with `docker run --rm ... backup` and use the plain CLI instead.

## API

### `POST /v1/backups`

Starts a backup run.

| Field | Required | Description |
| --- | --- | --- |
| `source` | yes | Path inside the container to archive. |
| `archive` | no | Directory the archive is written to. Defaults to `BACKUP_ARCHIVE`, or to `/archive`. It has to exist already, because the artifact on disk is what gets verified. |
| `job_id` | no | Correlation id. Defaults to a random id. Supplying the id of the approval request makes the backup traceable. |
| `volume` | no | Label for the volume being backed up. Reporting only. |
| `filename` | no | Overrides the file name. Defaults to `job-<job_id>.tar.gz`, so the artifact can be derived from the job id. |
| `retention_days` | no | Overrides `BACKUP_RETENTION_DAYS`. Accepted range is `1` to `BACKUP_MAX_RETENTION_DAYS` (default `365`); anything outside it is refused with `400`. A gate backup is never kept forever. |
| `wait` | no | When `true`, the request blocks until the run finishes and returns the final result. |

Asynchronous (default) response, `202 Accepted`:

```json
{"job_id":"approval-42","status":"pending"}
```

Synchronous (`"wait": true`) response, `200 OK`:

```json
{
  "job_id": "approval-42",
  "volume": "data",
  "status": "succeeded",
  "started_at": "2026-11-08T08:38:27.448920281Z",
  "finished_at": "2026-11-08T08:38:27.452610234Z",
  "duration_ms": 3,
  "artifact": {
    "path": "/archive/job-approval-42.tar.gz",
    "name": "job-approval-42.tar.gz",
    "size": 232,
    "sha256": "bb30a8b586f7515cb189c50380f9ead54a369c9032466330912dcf5d39de5ae2"
  }
}
```

### `GET /v1/backups/{job_id}`

Returns the state of a run.

```json
{"job_id":"approval-43","status":"succeeded","duration_ms":2,"artifact":{"path":"/archive/job-approval-43.tar.gz","name":"job-approval-43.tar.gz","size":232,"sha256":"bb30a8b..."}}
```

### `GET /healthz`

Returns `{"status":"ok"}` once the process is listening.

## Status codes

| Code | Meaning |
| --- | --- |
| `202` | Run accepted, poll `GET /v1/backups/{job_id}`. |
| `200` | Run finished successfully and the artifact was verified. |
| `400` | Invalid request: a missing or unreadable `source`, an `archive` that does not exist, a `filename` or `job_id` carrying a path separator or `..`, or a `retention_days` outside the allowed range. |
| `415` | The body was not sent as `Content-Type: application/json`. |
| `404` | Unknown `job_id`. |
| `409` | The supplied `job_id` is already known. |
| `500` | The run failed. The response body carries the error. |
| `504` | A synchronous request was cancelled while waiting. The run keeps going; poll by `job_id`. |

## Using the result as a gate

A gate should treat only `200` with a non-empty `artifact.path` as clearance:

```bash
curl -sf -X POST http://backup-api:8080/v1/backups \
  -H 'Content-Type: application/json' \
  -d '{"job_id":"approval-42","source":"/backup/target","archive":"/archive","wait":true}' \
  > /tmp/backup.json || exit 1
```

Because `curl -f` fails on `400`, `409`, `500` and `504`, the destructive action
is only reached when the backup was created, found on disk and hashed.

## Restoring

This fork does not add a restore command. Restoring is a manual procedure, see
[Restore volumes from a backup](./restore-volumes-from-backup.html) in the
upstream documentation.

Keep the gate's artifacts in a directory of their own, or pick a retention long
enough for the approval they protect: pruning removes every file older than the
retention period from the archive directory, including the artifact a rollback
depends on. Keeping an artifact forever is deliberately not an option - a request
may ask for at most `BACKUP_MAX_RETENTION_DAYS` (default 365) days, and a run
whose effective period is negative (the upstream default) or above the maximum is
clamped to the maximum and logs a warning. If something has to be archived longer
than that, copy it out of the archive directory.

## Concurrency

Runs are serialised by the same file lock the upstream scheduler uses
(`/var/lock/dockervolumebackup.lock`). The lock lives inside the container, so two
separate containers do not exclude each other. Set `LOCK_TIMEOUT` to a small value
in gate deployments so a request fails fast instead of waiting behind a long run.
