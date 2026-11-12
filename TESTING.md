# Local verification

A step-by-step check that the approval-triggered backup path works on a single
machine, ending with a **restore** so that "the backup can actually be rolled
back" is proven rather than assumed.

Run everything from a scratch directory. Nothing here touches an existing stack.

## What this proves

- the API accepts a trigger and returns a JSON result with an artifact digest;
- the digest the API reports matches the file on disk;
- invalid requests and failing runs are rejected instead of reported as success;
- the archive can be restored into a fresh volume and yields identical data.

## What this does not prove

- behaviour against real application volumes (quiescing, large volumes, slow I/O);
- remote storage backends (S3, SSH, WebDAV, ...);
- ordering under concurrent approvals from multiple hosts;
- that your image build works in CI.

## 1. Prerequisites

```console
$ docker version --format '{{.Server.Version}}'
$ jq --version
$ curl --version | head -1
```

If `jq` is missing: `sudo apt install -y jq`.

## 2. Build the image

```console
$ mkdir -p /tmp/soteria-test && cd /tmp/soteria-test
$ git clone https://github.com/seoulorigin/soteria.git src
$ cd src && docker build -t soteria:test .
```

The first build pulls `golang:1.27-alpine` and `alpine:3.24` and takes a few
minutes. If this step fails, stop here: nothing below can work.

## 3. Create test data

```console
$ docker volume create soteria_test_data
$ docker run --rm -v soteria_test_data:/data alpine sh -c '
    echo "critical-row-1" > /data/db.sql
    mkdir -p /data/nested && echo "nested-payload" > /data/nested/f.txt
  '
```

Record a fingerprint of the data so the restore can be compared later:

```console
$ docker run --rm -v soteria_test_data:/data alpine \
    sh -c 'find /data -type f | sort | xargs sha256sum' | tee /tmp/soteria-test/before.txt
```

Two lines of `sha256sum` output means the fixture is in place.

## 4. Start the API

```console
$ mkdir -p /tmp/soteria-test/archive && chmod 777 /tmp/soteria-test/archive

$ docker run -d --name soteria-test \
    -p 127.0.0.1:18080:8080 \
    -e LOCK_TIMEOUT=10s \
    -v soteria_test_data:/backup/target:ro \
    -v /tmp/soteria-test/archive:/archive \
    --entrypoint /usr/bin/backup \
    soteria:test serve -http-addr :8080

$ sleep 2 && curl -s http://127.0.0.1:18080/healthz
{"status":"ok"}
```

`-http-addr` defaults to `:8080` and is read whether it is passed before or after
`serve`; if you change it, change the port mapping too.
If `healthz` does not answer, check `docker logs soteria-test` — the usual causes
are the `/archive` mount and its permissions.

## 5. Happy path

Write the body to a file with `-o` and print the status code with `-w`. Do **not**
pipe `-w` output into the same file: it appends a `HTTP=200` line and the file is
no longer valid JSON.

```console
$ curl -s -o /tmp/soteria-test/resp.json -w 'HTTP=%{http_code}\n' \
    -X POST http://127.0.0.1:18080/v1/backups \
    -H 'Content-Type: application/json' \
    -d '{"job_id":"local-test-1","volume":"soteria_test_data",
         "source":"/backup/target","archive":"/archive","wait":true}'
HTTP=200
```

```console
$ jq -r '.status, .artifact.name, .artifact.size' /tmp/soteria-test/resp.json
succeeded
job-local-test-1.tar.gz
<size in bytes>
```

Omitting `archive` is allowed: the API falls back to the container's default
(`/archive`), so a minimal body only names the source.

```console
$ curl -s -o /tmp/soteria-test/resp2.json -w 'HTTP=%{http_code}\n' \
    -X POST http://127.0.0.1:18080/v1/backups \
    -H 'Content-Type: application/json' \
    -d '{"job_id":"local-test-2","source":"/backup/target","wait":true}'
HTTP=200
```

## 6. Verify the artifact independently

This is the step that matters. The digest reported by the API has to match the
file that is actually on disk, otherwise the report cannot be used as a gate.

```console
$ sha256sum /tmp/soteria-test/archive/job-local-test-1.tar.gz
$ jq -r '.artifact.sha256' /tmp/soteria-test/resp.json
```

Both commands must print the **same** hex string. If they differ, stop: the
service would authorise a destructive action based on a value nobody checked.

Optionally confirm the archive content:

```console
$ tar -tzf /tmp/soteria-test/archive/job-local-test-1.tar.gz
backup/target
backup/target/db.sql
backup/target/nested/
backup/target/nested/f.txt
```

## 7. Negative cases

A gate is only worth something if the failures fail.

```console
$ P=http://127.0.0.1:18080/v1/backups
$ H='Content-Type: application/json'

# unreadable source -> 400
$ curl -s -w ' HTTP=%{http_code}\n' -X POST $P -H "$H" \
    -d '{"source":"/nope","archive":"/archive","wait":true}'

# archive that does not exist -> 400
$ curl -s -w ' HTTP=%{http_code}\n' -X POST $P -H "$H" \
    -d '{"source":"/backup/target","archive":"/nope","wait":true}'

# a file name that could leave the archive directory -> 400
$ curl -s -w ' HTTP=%{http_code}\n' -X POST $P -H "$H" \
    -d '{"source":"/backup/target","archive":"/archive","filename":"../x.tar.gz","wait":true}'

# a retention period the gate does not allow (forever) -> 400
$ curl -s -w ' HTTP=%{http_code}\n' -X POST $P -H "$H" \
    -d '{"source":"/backup/target","archive":"/archive","retention_days":-1,"wait":true}'

# a body that is not declared as JSON -> 415
$ curl -s -w ' HTTP=%{http_code}\n' -X POST $P \
    -d '{"source":"/backup/target","archive":"/archive"}'

# duplicate job_id -> 409
$ curl -s -w ' HTTP=%{http_code}\n' -X POST $P -H "$H" \
    -d '{"job_id":"local-test-1","source":"/backup/target","archive":"/archive"}'

# unknown job -> 404
$ curl -s -w ' HTTP=%{http_code}\n' http://127.0.0.1:18080/v1/backups/nope
```

Expected: `400` (unreadable source), `400` (archive that does not exist), `400`
(filename), `400` (retention), `415` (media type), `409` (duplicate job id),
`404` (unknown job).

To exercise the "run fails" path, restart the container with a read-only archive
and request a backup: the response is `500` and the body carries the underlying
error.

## 8. Restore cycle

```console
# the destructive action the gate was protecting
$ docker stop soteria-test && docker rm soteria-test
$ docker volume rm soteria_test_data

# restore from the verified artifact
$ docker volume create soteria_test_data
$ docker run --rm \
    -v soteria_test_data:/restore \
    -v /tmp/soteria-test/archive:/archive:ro \
    alpine tar -xzf /archive/job-local-test-1.tar.gz -C /restore --strip-components=2

# compare against the fingerprint taken in step 3
$ docker run --rm -v soteria_test_data:/data alpine \
    sh -c 'find /data -type f | sort | xargs sha256sum' | tee /tmp/soteria-test/after.txt
$ diff /tmp/soteria-test/before.txt /tmp/soteria-test/after.txt && echo RESTORE_OK
RESTORE_OK
```

`--strip-components=2` matches the archive layout produced by
`source=/backup/target`: entries are `backup/target/...`, so two leading
components are stripped. If you use a different source path, check the depth
first with `tar -tzf`.

## 9. Clean up

```console
$ docker rm -f soteria-test 2>/dev/null
$ docker volume rm soteria_test_data 2>/dev/null
$ rm -rf /tmp/soteria-test
```

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `healthz` does not answer | port mismatch, or the container failed to start — check `docker logs` |
| `400` "archive ... does not exist" | the host directory was not created, or you passed a host path instead of `/archive` |
| `500` "permission denied" | the archive directory is not writable by the container user |
| `400` "source ... is not readable" | the volume is not mounted at the given `source` path |
| `docker volume rm` refuses | a container still mounts the volume — remove the container first |
| `jq` "Invalid numeric literal" | `curl -w` output was written into the JSON file; use `-o` for the body |
| build fails | network or module download; retry with `docker build --no-cache .` |