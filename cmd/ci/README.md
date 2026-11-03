# Chamber CI

A rootless Linux application built directly on the Chamber SDK. There is no
daemon dependency or built-in CI pipeline: a repository's `Dockerfile.chamber`
defines its build and default command. Exit zero passes; a nonzero exit or image
build failure fails. Checkout/runtime failures, interruption, panics, and timeouts
produce an error status.

For example, a Go repository can provide this root `Dockerfile.chamber`:

```dockerfile
FROM golang:1.27.3-trixie
WORKDIR /workspace
COPY . .
CMD ["go", "test", "-count=1", "./..."]
```

## Install once

Use a Linux user with working rootless user namespaces and subordinate UID/GID
mappings, as required by the SDK. Install Git and Go 1.26.4 or newer; Go is needed
for self-update. Chamber manages its BuildKit, RootlessKit, and runc binaries.
The optional `buildkit` object uses the fields of `image.BuildKitConfig`;
`runtime_path` can select a user-owned local runc executable.

```sh
mkdir -p ~/.local/bin ~/.config/chamber-ci ~/.config/systemd/user
go build -o ~/.local/bin/chamber-ci ./cmd/ci
cp cmd/ci/config.example.json ~/.config/chamber-ci/config.json
cp cmd/ci/ci.service ~/.config/systemd/user/chamber-ci.service
```

Edit the config's root, public origin, repository names, and update source.
Use a local Linux filesystem for private state (0700), with working file and
directory fsync and atomic rename. The binary directory must remain user-writable.
Execution workspaces default to `/tmp/chamber-ci-<uid>` to keep BuildKit's Unix
socket paths below Linux's length limit; `work_root` can select another short,
private path. Image and temporary build files stay together on that filesystem.
Keep `root` and `work_root` stable while any job still needs cleanup; startup
rejects a changed workspace location instead of abandoning existing resources.
A file lock in startup prevents two servers using the same state directory:
otherwise one could mistake the other's running jobs for dropped jobs.

Create `~/.config/chamber-ci/secrets.env` (0600):

```ini
CHAMBER_GITHUB_TOKEN=your-token
CHAMBER_WEBHOOK_SECRET=your-random-webhook-secret
```

The GitHub token needs permission to write commit statuses on each configured
repository. It is used only for status API requests. Private checkout uses the
user's Git credential helper or SSH agent; it does not reuse the status token.
For SSH, set `clone_url` to an `ssh://git@host/owner/repo.git` URL and arrange a
known host key and noninteractive credentials. Git/Go subprocesses receive a
small environment without the configured status tokens or webhook secrets.

```sh
systemctl --user daemon-reload
systemctl --user enable --now chamber-ci
```

These are initial installation commands. Updates never invoke systemd. For
unattended operation after logout, enable user lingering through your host's
normal account setup. Place a TLS reverse proxy in front of the configured
loopback listener; `public_url` is the externally reachable origin.

## GitHub webhook and logs

For each repository key, configure a JSON GitHub webhook at
`https://ci.example.com/webhooks/<key>`, with its configured secret. Subscribe to
pushes and pull requests. Pushes to branches and PR actions `opened`,
`synchronize`, `reopened`, and `ready_for_review` trigger jobs. Deleted branches,
tags, pings, and unrelated events are ignored. GitHub Enterprise can use explicit
`clone_url` and `api_url` values. Only GitHub is currently implemented.

Every accepted job is committed to `root/jobs.sqlite` before returning HTTP 202.
The queue is bounded; a full queue returns 429 and shutdown returns 503. Repeated
deliveries for the same configured repository and SHA return the existing job (200), so one
commit runs once even when both push and PR events arrive. A rejected webhook
needs redelivery; GitHub does not automatically retry failed deliveries.

The store also gates execution. One atomic SQLite statement counts `running`
rows and changes the oldest `queued` row to `running` only when there is room
under `max_parallel`. The server starts execution only after that claim succeeds.
There is no in-memory job queue, semaphore, or fixed worker pool. Goroutines run
claimed jobs; they do not decide how many jobs may run. A slot is released only
when the terminal result is committed, including when a write must be retried.

The checkout must match the event's exact SHA. A moving branch cannot substitute
a newer commit. The root `Dockerfile.chamber` must be a regular file. Checkout,
image storage, build state, and runtime resources use a separate private workspace
per job. The built image runs its own entrypoint and command with no checkout or
host credentials mounted into it. Configure the image itself to provide anything
the command needs. Managed tools are also per-job; explicit local tool paths can
avoid repeated downloads.

GitHub receives commit statuses under context `chamber`, with a `target_url` of
`/runs/<id>`. That page polls `/runs/<id>/log` every two seconds. Clone/build
diagnostics and container stdout/stderr share a bounded text log (default 16 MiB;
further output is discarded). Logs and records remain after workspace cleanup.
The log page is public to anyone who has its URL, including for private repos;
protect the viewer at your proxy if needed. Put no secrets in build output.
The page heading is a snapshot; refresh the page to update its displayed outcome.

Jobs run for at most `job_timeout` (default 30 minutes, followed by cleanup).
On startup, interrupted running jobs become errors. Queued jobs remain queued.
Status updates and workspace cleanup are retried. Normal shutdown cancels running
jobs and marks queued jobs errored; an update preserves queued jobs for the new process. Normal status API failures
are retried every five seconds, with durable acknowledgement only after delivery.
Restore a removed or renamed repository's config if it still has unreconciled jobs.
Repository keys are stable identities; use a new key for a different repository. A server
that stays offline cannot deliver statuses until it starts again.

Records and logs have no automatic expiry. Job records live in SQLite; logs and
the update passcode remain private files. SQLite uses WAL mode and full sync.
Stop CI before taking a filesystem backup of its complete state directory.
This replaces the earlier JSON-record prototype; no JSON import is provided.

To reclaim an individual job, stop CI, verify that its row has terminal state
and both `reported` and `cleaned` equal 1, then remove the row and matching
`logs/<id>.log`. Keep `update.passcode`. Deleting a row removes that commit's
deduplication. The application does not currently expose a retention command.

## Self-update

The first normal startup creates `root/update.passcode`: 32 random bytes encoded
as 64 hex characters, stored privately. Existing passcodes are reused. An initialization marker makes a missing passcode
an error on later startups; missing or invalid credentials are never replaced. The
passcode is never printed to logs. Read it locally to authorize an update:

```sh
passcode=$(cat ~/.local/state/chamber-ci/update.passcode)
curl --fail-with-body -X POST https://ci.example.com/update \
  -H "Authorization: Bearer $passcode"
```

The endpoint returns 202; concurrent update requests return 409. It fetches the
configured update branch into a temporary checkout and builds `./cmd/ci` on the host.
Only this explicitly trusted source can
replace CI; an ordinary CI webhook never triggers an update. Follow progress in
`root/update.log` and the service's journal. Preparation failures leave the current
server accepting work.

Before activation, the candidate parses the config, checks existing state without
changing jobs or credentials, and serves a temporary loopback `/health` endpoint.
The current server requests that endpoint with a 30-second deadline. A failed
check leaves the current server serving and executing jobs.

After a successful check, new webhook jobs continue to enter SQLite while running
jobs finish. Queued jobs wait for the new process. CI stops its background work,
makes a bounded final status-delivery attempt, closes SQLite, and calls Linux
`exec`. The listening socket and state lock stay open across that call. The PID,
port, queued jobs, and passcode are retained. New connections can wait in the
kernel backlog during startup; existing HTTP connections may close.

No supervisor or systemd update calls are involved. If `exec` fails, CI restores
the previous binary and resumes on the retained socket. A successful update keeps
`<binary>.previous` for manual rollback. The startup health check does not provide
automatic rollback for a later crash. Reboot recovery uses the installed executable
and normal service setup.

Keep the update endpoint behind HTTPS and protect the passcode like a deployment
credential: it authorizes running code from the configured update source as the
CI user. Configuration and durable record formats must be readable by the
candidate; future incompatible changes require explicit migrations.

## Validation

The code follows the application steps directly:

- `main.go` owns startup, the state lock, the retained socket, and process replacement.
- `server.go` owns HTTP handlers, job execution, status retries, and shutdown.
- `executor.go` checks out a commit, builds its image, waits for the container, and deletes it.
- `store.go` defines jobs and the storage contract; `sqlite.go` implements it.
- `files.go` owns the persistent password, atomic writes, and bounded logs.
- `update.go` builds and installs the replacement; `preflight.go` checks candidate startup and health.

Shutdown has a 45-second deadline. An uncooperative task causes process exit;
its durable running row is recovered on the next startup. Terminal statuses get
a final delivery attempt after tasks stop. Failed deliveries stay in SQLite for
retry after startup. The deadline also bounds the final attempt.

To change the backend, implement `jobStore` and change its construction in
`main.go`. The store contract tests can be reused. Admission and claims must
remain atomic across independent connections; a process-local mutex is insufficient.

Run on Linux or Lima:

```sh
GOCACHE=/tmp/chamber-go-cache go test -race ./cmd/ci
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -v ./cmd/ci -run Integration
```

The default tests cover webhook authentication, admission, deduplication, job
failure/panic/timeout, status ordering/retries, recovery, bounded logs, and update
authentication. SQLite tests contend through independent connections, verify
queue/execution limits, retry failed terminal writes, reject stale status
acknowledgements, and reopen the database to verify durability. Opt-in tests
exercise actual process replacement, candidate rejection, listener continuity,
and rootless Dockerfile execution.
See test output for which opt-in cases were run.

GitHub's contracts: [webhook payloads](https://docs.github.com/en/webhooks/webhook-events-and-payloads),
[commit statuses](https://docs.github.com/en/rest/commits/statuses), and
[failed delivery handling](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries).
SQLite's [transaction rules](https://www.sqlite.org/lang_transaction.html) explain
why counting and claiming must happen in the same write operation.
