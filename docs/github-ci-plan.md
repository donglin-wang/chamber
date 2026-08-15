# Chamber Dogfood CI Webhook Plan

This plan describes the first Chamber-hosted CI system for Chamber itself:

1. GitHub sends a signed webhook for a pushed git ref.
2. A small Linux webhook receiver validates the event and admits it only if a
   process-local concurrency slot is available.
3. The worker checks out the exact commit SHA from the payload.
4. The worker runs Chamber's existing container-based CI flow against that
   checkout.
5. The worker reports a GitHub commit status for the tested SHA.

V1 intentionally uses Chamber containers on one Linux VM. It does not spawn a
nested VM per CI run. A per-run VM is a later isolation milestone because the
current repo does not have an implemented `pkg/machine` or VM abstraction.

## Platform Boundary

Chamber only supports Linux. There is no macOS support for build, provision,
runtime, or CI validation.

macOS is only a development host. When working from macOS, run meaningful
Chamber validation inside Linux with Lima:

```sh
limactl shell <linux-instance> --workdir /Users/donglinwang/Projects/chamber -- env GOCACHE=/tmp/chamber-go-cache go test ./pkg/... ./cmd/...
limactl shell <linux-instance> --workdir /Users/donglinwang/Projects/chamber -- env CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./internal/ci -run TestRunDogfoodIntegration
```

Do not add macOS compatibility shims to make this CI path run natively on
macOS.

## Hosting Choice

Use **Oracle Cloud Infrastructure Always Free**:

- shape: `VM.Standard.A1.Flex`;
- image: Ubuntu ARM64;
- size: 2 OCPUs and 12 GB RAM;
- disk: 100 GB boot volume;
- network: one public IPv4 address;
- region: the OCI account's home region.

Why this choice:

- OCI documents Always Free Arm compute as 1,500 OCPU hours and 9,000 GB hours
  per month, equivalent to 2 OCPUs and 12 GB memory for Always Free tenancies.
- OCI also includes 200 GB total Always Free block volume storage, which is
  enough for a small image store, bundle roots, runtime logs, Go caches, and
  checkouts.
- Chamber already has managed ARM64 BuildKit, RootlessKit, and runc artifact
  paths, and Linux ARM64 compile currently passes.

Avoid Fly.io for V1 free hosting. Fly's current public docs describe a short
free trial, not a durable always-free VM. It is a good app platform, but a poor
fit for an always-on webhook receiver that needs rootless container runtime host
features.

Sources checked while writing this plan:

- OCI Always Free resources:
  `https://docs.oracle.com/en-us/iaas/Content/FreeTier/freetier_topic-Always_Free_Resources.htm`
- GitHub webhook signature validation:
  `https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries`
- GitHub webhook payloads:
  `https://docs.github.com/en/webhooks/webhook-events-and-payloads`
- GitHub commit statuses:
  `https://docs.github.com/en/rest/commits/statuses`
- Fly.io free trial:
  `https://fly.io/docs/about/free-trial/`

## Target Architecture

Add one new binary:

```text
cmd/github-ci
```

Keep reusable CI runner code in:

```text
internal/ci
```

There is no separate `cmd/ci` CLI in V1. The dogfood runner is exercised by the
opt-in `TestRunDogfoodIntegration` test with fixed defaults.

Keep public `pkg/` APIs unchanged for V1. The webhook worker should compose the
existing SDK primitives:

- `pkg/image` and `pkg/image/factory` for the Go test image;
- `pkg/bundle` and `pkg/bundle/factory` for workspace/cache bind mounts;
- `pkg/runtime` and `pkg/runtime/factory` for runc-backed execution;
- `pkg/shared/hostfs` for caller-owned durable and temporary roots;
- `pkg/shared/logging` for JSON logs;
- `pkg/shared/subprocess` for host-side `git` and helper commands.

The webhook process owns runner policy:

- signature validation;
- repository allowlist;
- process-local parallelism gating;
- checkout lifecycle;
- Chamber root placement;
- log retention;
- GitHub status updates.

Do not move those responsibilities into low-level `pkg/` SDK packages.

## Filesystem Layout On The VM

Use one unprivileged Linux user:

```text
chamberci
```

Use these paths:

```text
/opt/chamber-ci/src                 # deployed source checkout for the receiver binary
/usr/local/bin/github-ci            # built webhook receiver
/etc/chamber-ci/webhook.env         # secrets and config
/var/lib/chamber-ci                 # all mutable CI state
/var/log/chamber-ci                 # systemd-accessible service logs if needed
```

Mutable runner state:

```text
/var/lib/chamber-ci/runs/<run-id>/checkout
/var/lib/chamber-ci/runs/<run-id>/logs
/var/lib/chamber-ci/ci/tmp/runs/chamber-ci-*/images
/var/lib/chamber-ci/ci/tmp/runs/chamber-ci-*/bundles
/var/lib/chamber-ci/ci/tmp/runs/chamber-ci-*/run/runtime
/var/lib/chamber-ci/ci/tmp/runs/chamber-ci-*/bin
/var/lib/chamber-ci/ci/tmp/runs/chamber-ci-*/tmp
/var/lib/chamber-ci/chamber-root          # startup preflight state
```

Keep all Chamber package roots, temp roots, logs, and checkouts below
`/var/lib/chamber-ci`. Do not use ambient `/tmp` for runner-owned state.

## OCI VM Setup

Create the VM in OCI Console:

1. Create an Always Free `VM.Standard.A1.Flex` instance.
2. Select Ubuntu ARM64.
3. Set shape resources to 2 OCPUs and 12 GB RAM.
4. Set boot volume to 100 GB.
5. Put the instance in a public subnet.
6. Attach an SSH key.
7. Add ingress rules for:
   - `22/tcp` from your own IP only;
   - `8080/tcp` from `0.0.0.0/0`.
8. Deny all other inbound traffic.

Bootstrap the host:

```sh
sudo apt-get update
sudo apt-get install -y git curl ca-certificates uidmap slirp4netns fuse-overlayfs jq

sudo adduser --disabled-password --gecos "" chamberci
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 chamberci

sudo mkdir -p /opt/chamber-ci /var/lib/chamber-ci /var/log/chamber-ci /etc/chamber-ci
sudo chown -R chamberci:chamberci /opt/chamber-ci /var/lib/chamber-ci /var/log/chamber-ci
sudo chmod 0750 /etc/chamber-ci
```

Install Go `1.26.4` ARM64:

```sh
cd /tmp
curl -LO https://go.dev/dl/go1.26.4.linux-arm64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.26.4.linux-arm64.tar.gz
echo 'export PATH=/usr/local/go/bin:$PATH' | sudo tee /etc/profile.d/go.sh
```

Clone and build the receiver:

```sh
sudo -iu chamberci
cd /opt/chamber-ci
git clone https://github.com/donglin-wang/chamber.git src
cd src
GOCACHE=/var/lib/chamber-ci/go-build-host /usr/local/go/bin/go build -o /tmp/github-ci ./cmd/github-ci
exit

sudo install -o root -g root -m 0755 /tmp/github-ci /usr/local/bin/github-ci
```

## Receiver Configuration

Create `/etc/chamber-ci/webhook.env`:

```sh
CHAMBER_CI_ADDR=0.0.0.0:8080
CHAMBER_CI_STATUS_TARGET_BASE_URL=http://<vm-public-ip>:8080
CHAMBER_CI_ROOT=/var/lib/chamber-ci
CHAMBER_CI_REPOSITORY=donglin-wang/chamber
CHAMBER_CI_GITHUB_TOKEN=<fine-grained-token-with-commit-status-write>
MAX_PARALLEL=1
CHAMBER_CI_RUN_TIMEOUT=30m
CHAMBER_CI_RETENTION=168h
```

Permissions:

```sh
sudo chown root:chamberci /etc/chamber-ci/webhook.env
sudo chmod 0640 /etc/chamber-ci/webhook.env
```

`CHAMBER_CI_STATUS_TARGET_BASE_URL` is the externally reachable base URL used
for the clickable `target_url` on GitHub commit statuses. It points back to this
receiver, not to GitHub.

`CHAMBER_CI_GITHUB_TOKEN` is used both as the GitHub webhook secret and as the
Bearer token for commit status writes.

## Systemd Service

Create `/etc/systemd/system/github-ci.service`:

```ini
[Unit]
Description=Chamber dogfood CI webhook receiver
After=network-online.target
Wants=network-online.target

[Service]
User=chamberci
Group=chamberci
WorkingDirectory=/var/lib/chamber-ci
EnvironmentFile=/etc/chamber-ci/webhook.env
ExecStart=/usr/local/bin/github-ci -addr ${CHAMBER_CI_ADDR}
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/chamber-ci /var/log/chamber-ci

[Install]
WantedBy=multi-user.target
```

Start it:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now github-ci
sudo systemctl status github-ci
```

## GitHub Webhook Setup

In the GitHub repository:

1. Go to Settings -> Webhooks -> Add webhook.
2. Payload URL: `http://<vm-public-ip>:8080/github/webhook`.
3. Content type: `application/json`.
4. Secret: the exact value from `CHAMBER_CI_GITHUB_TOKEN`.
5. Events: select only `push`.
6. Active: enabled.

Receiver behavior:

- accept only `POST /github/webhook`;
- require `X-GitHub-Event: push`;
- require `X-GitHub-Delivery`;
- require `X-Hub-Signature-256`;
- compute `sha256=<hex-hmac>` over the raw request body;
- compare signatures in constant time;
- reject payloads whose repository full name is not `donglin-wang/chamber`;
- ignore deleted refs;
- try to acquire one `MAX_PARALLEL` slot;
- return `429 Too Many Requests` when all slots are full;
- create a run directory and start one goroutine when a slot is acquired;
- return `202 Accepted` with the run ID for admitted work.

## In-Memory Parallelism Gate

Do not add SQLite or a durable run queue for V1. The receiver is a single
process with one in-memory concurrency limit:

```go
var MAX_PARALLEL = 1
var runSlots = make(chan struct{}, MAX_PARALLEL)
```

Admission logic:

```go
func tryAcquireRunSlot() bool {
	select {
	case runSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseRunSlot() {
	<-runSlots
}
```

Request flow:

1. Validate HTTP method, GitHub headers, HMAC signature, repository, and payload.
2. If `tryAcquireRunSlot()` returns false, return `429 Too Many Requests` and
   do not create a run.
3. If a slot is acquired, create `runID`, checkout directory, and log directory.
4. Start a goroutine for checkout, Chamber CI, log writes, and status updates.
5. `defer releaseRunSlot()` inside that goroutine.
6. Return `202 Accepted` with the run ID immediately after the goroutine starts.

This means V1 has no durable dedupe and no restart recovery. If GitHub redelivers
the same event while a slot is available, Chamber CI may run the same SHA again.
That is acceptable for the first dogfood version.

## Checkout Flow

For each run:

```sh
mkdir -p /var/lib/chamber-ci/runs/<run-id>/checkout
cd /var/lib/chamber-ci/runs/<run-id>/checkout
git init
git remote add origin https://github.com/donglin-wang/chamber.git
git fetch --depth=1 origin <after-sha>
git checkout --detach FETCH_HEAD
test "$(git rev-parse HEAD)" = "<after-sha>"
```

Use `pkg/shared/subprocess.CommandContext` for `git` commands so the child
process inherits Chamber's subprocess safety defaults.

Do not run arbitrary git refs as shell text. The worker must treat the payload's
`after` field as data and pass it as one argument to `git fetch`.

## Chamber CI Flow

After checkout, run the same logical jobs as `internal/ci`:

```text
pkg:  go test ./pkg/...
full: go test ./...
```

Use the existing Go image default from `internal/ci`:

```text
docker.io/library/golang:1.26.4-bookworm
```

The runner should call the extracted `internal/ci` package with:

```text
root:    /var/lib/chamber-ci/ci
workdir: /var/lib/chamber-ci/runs/<run-id>/checkout
image:   docker.io/library/golang:1.26.4-bookworm
timeout: 30m
keep:    false
```

The current CI spine already:

- creates package-specific `hostfs.Workspace` values;
- pulls the Go image with `image.Store`;
- provisions bundles with workspace and Go cache bind mounts;
- runs containers through runc;
- reads stdout and stderr logs after completion;
- deletes runtime containers and logs when `keep=false`.

The webhook runner should store a copy of each job's stdout/stderr under:

```text
/var/lib/chamber-ci/runs/<run-id>/logs/<job>.stdout
/var/lib/chamber-ci/runs/<run-id>/logs/<job>.stderr
```

## GitHub Status Reporting

Use the REST commit status endpoint:

```text
POST /repos/donglin-wang/chamber/statuses/<sha>
```

Context:

```text
chamber-ci
```

Transitions:

```text
admitted/running -> pending
succeeded        -> success
failed           -> failure
errored          -> error
```

Descriptions:

```text
pending: Chamber CI is running on OCI A1 ARM64
success: Chamber CI passed
failure: Chamber CI failed
error: Chamber CI errored before tests completed
```

Set `target_url` to:

```text
http://<vm-public-ip>:8080/runs/<run-id>
```

V1 can serve a plain text or JSON run page. It does not need a UI.

## HTTP Endpoints

Implement only these endpoints:

```text
POST /github/webhook
GET  /healthz
GET  /runs/<run-id>
GET  /runs/<run-id>/logs/<job>/stdout
GET  /runs/<run-id>/logs/<job>/stderr
```

`GET /healthz` returns `200 OK` only when:

- the process is running;
- the configured root exists and is writable.

Run log endpoints should not expose arbitrary paths. Resolve logs from the run
ID and job name, validate both path components, and read only below:

```text
/var/lib/chamber-ci/runs/<run-id>/logs
```

## Current Repo Gaps To Close First

### 1. Preserve the Linux-only contract

`pkg/shared/subprocess` uses Linux parent-death signaling for Chamber-spawned
processes. That is correct for Chamber's supported platform.

Do not add macOS compatibility shims for this CI path. If a developer is on
macOS, they should validate this plan through `limactl shell` into a Linux
guest.

### 2. Keep the reusable runner in `internal/ci`

`internal/ci` owns config assembly, image pull, bundle provisioning, runtime
execution, logging, and result aggregation.

The reusable runner shape is:

```go
type Config struct {
    Root    string
    Workdir string
    Image   string
    Timeout time.Duration
    Keep    bool
}

type Job struct {
    Name string
    Args []string
}

type Result struct {
    ExitCode int
    Jobs     []JobResult
}

type JobResult struct {
    Name     string
    ExitCode int
    Stdout   []byte
    Stderr   []byte
    Err      error
}

func Run(ctx context.Context, cfg Config) (Result, error)
```

The dogfood end-to-end path is an opt-in integration test, not a parameterized
CLI. It uses:

```text
root:    /var/tmp/chamber-ci-<uid>
workdir: repository root
image:   docker.io/library/golang:1.26.4-bookworm
timeout: 30m
keep:    false
```

### 3. Add a host preflight before exposing the webhook

The repo has `docs/host-assumption-validator-plan.md`, but no implemented
validator yet.

For V1, add a small runner-local preflight in `cmd/github-ci`:

- host is Linux;
- current user is not root;
- `/etc/subuid` and `/etc/subgid` contain `chamberci`;
- `newuidmap` and `newgidmap` are available;
- Chamber root is private and writable;
- `git` is available;
- managed runc can be installed or found;
- a tiny Chamber container can run and exit.

This can later move into a real public validator package once the shape in
`docs/host-assumption-validator-plan.md` is implemented.

### 4. Keep V1 bounded by `MAX_PARALLEL`

The SDK caller owns concurrency and cleanup today. The webhook worker should run
only `MAX_PARALLEL` admitted runs at a time until Chamber has daemon-grade
operation records, leases, and recovery for shared roots.

Default `MAX_PARALLEL` to `1`. Higher values are allowed by config, but the
operator owns the risk of concurrent mutation below one Chamber root.

### 5. Document network behavior

The directory provisioner currently removes the OCI network namespace in the
rootless spec. That means CI jobs can usually use host networking, which helps
`go test` download modules.

This is acceptable for V1 against trusted pushes to the Chamber repo. It is not
acceptable for arbitrary untrusted fork execution without a stronger network and
secret-isolation model.

## Implementation Order

1. Keep Chamber CI validation Linux-only; use Lima from macOS.
2. Keep `internal/ci` as the reusable runner and gate dogfood execution behind `CHAMBER_INTEGRATION=1`.
3. Add `cmd/github-ci` with config loading, health endpoint, and
   signature validation.
4. Add the in-memory `MAX_PARALLEL` admission gate.
5. Add checkout logic using `git` through `pkg/shared/subprocess`.
6. Wire `internal/ci.Run` into the worker.
7. Add GitHub commit status updates.
8. Add run/log HTTP endpoints.
9. Deploy to OCI and run the manual validation checklist.
10. Configure the GitHub webhook and redeliver one event.

## Test Plan

Unit tests:

- HMAC-SHA256 signature validation accepts GitHub's documented shape and rejects
  missing or mismatched signatures.
- Receiver rejects non-`push` events.
- Receiver rejects repositories other than `donglin-wang/chamber`.
- Receiver ignores deleted refs.
- Receiver returns `429 Too Many Requests` when all `MAX_PARALLEL` slots are
  occupied.
- Receiver releases a `MAX_PARALLEL` slot after success, failure, or error.
- Checkout command builder fetches the exact `after` SHA as one argument.
- Status client maps runner outcomes to GitHub status states.
- Run log endpoints cannot escape the run log directory.

Linux compile and test checks:

```sh
GOCACHE=/tmp/chamber-go-cache go test -run '^$' ./pkg/... ./cmd/...
```

When working from macOS, run those checks inside Lima:

```sh
limactl shell <linux-instance> --workdir /Users/donglinwang/Projects/chamber -- env GOCACHE=/tmp/chamber-go-cache go test ./pkg/... ./cmd/...
```

OCI Linux VM checks:

```sh
GOCACHE=/tmp/chamber-go-cache go test ./pkg/... ./cmd/...
CHAMBER_INTEGRATION=1 GOCACHE=/tmp/chamber-go-cache go test -count=1 ./internal/ci -run TestRunDogfoodIntegration
```

End-to-end checks:

1. Start `github-ci` under systemd.
2. Confirm `GET http://<vm-public-ip>:8080/healthz` returns `200`.
3. Send GitHub's webhook redelivery from the repository settings page.
4. Confirm the receiver returns `202 Accepted`.
5. Confirm a run directory exists under `/var/lib/chamber-ci/runs/<run-id>`.
6. Confirm the checkout HEAD equals the payload's `after` SHA.
7. Confirm the worker creates a pending GitHub status.
8. Confirm the worker runs `pkg` then `full`.
9. Confirm stdout/stderr logs are readable from `/runs/<run-id>`.
10. Confirm the final GitHub status is `success`, `failure`, or `error`.
11. With `MAX_PARALLEL=1`, send two valid webhooks close together and confirm
    the second returns `429 Too Many Requests` while the first is running.

## Acceptance Criteria

- A push to `donglin-wang/chamber` starts one run for the exact pushed commit
  when a `MAX_PARALLEL` slot is available.
- A valid push receives `429 Too Many Requests` when no `MAX_PARALLEL` slot is
  available.
- The worker checks out the exact `after` SHA and refuses to run if HEAD differs.
- The worker runs the Chamber CI jobs inside Chamber-launched containers.
- The GitHub commit receives a `chamber-ci` status.
- All mutable state stays under `/var/lib/chamber-ci`.
- `CHAMBER_CI_GITHUB_TOKEN` is required and verified before payload parsing is trusted.
- The plan treats Chamber as Linux-only; macOS validation goes through Lima.

## Future Milestones

After V1 is stable:

- Add a second AMD64 worker if Always Free E2 micro capacity is sufficient or if
  a small paid VM is acceptable.
- Add a real host assumption validator package based on
  `docs/host-assumption-validator-plan.md`.
- Add optional live log streaming.
- Add per-run VM isolation only after Chamber has a deliberate machine/VM
  package again.
- Move from personal token statuses to a GitHub App with narrower installation
  permissions.
- Add lease-aware cleanup and crash recovery in `chamberd` before running
  untrusted external contributions.
