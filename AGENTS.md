# AGENTS.md — lattice-runner

> `lattice-runner` is the **worker-agent daemon for Lattice**, the container orchestration
> platform that runs every `appleby.cloud` service. One instance runs on **each worker VM**.
> It dials **outbound** to [`lattice-api`](https://github.com/aidenappl/lattice-runner) over a
> single persistent **WebSocket** (no inbound ports required) and, on the other side of that
> socket, drives the local **Docker daemon**: it deploys stacks, runs container lifecycle
> actions (start/stop/restart/kill/pause/recreate/remove), streams logs, reports host and
> container metrics, manages database containers, runs scheduled snapshots to remote backup
> destinations, and exposes a small read-only local status dashboard.
>
> It is the **data plane**. It owns *no* state and makes *no* decisions about *what* should
> run — every instruction arrives as a message from `lattice-api`, the **control plane**. The
> runner's whole job is to execute those messages against Docker and report back.
>
> **⚠️ Golden rule — keep this file current:** the message protocol in *Domain & architecture*
> is a **contract with `lattice-api`**. Any change to a message type (inbound or outbound), its
> payload fields, the deploy spec shape, the config/env surface, or the Docker interaction model
> MUST update this AGENTS.md in the SAME change. A stale protocol table here silently breaks the
> worker fleet. If you finish work and haven't touched AGENTS.md, confirm that's actually correct.

---

## What this repo is

A single Go binary (`lattice-runner`) that is installed — usually as a `systemd` service — on
every machine that Lattice schedules containers onto. On startup it:

1. Loads config from the environment (`config.Load`).
2. Connects to the local Docker Engine API (retrying up to 30× at 2s intervals).
3. Opens an outbound WebSocket to `lattice-api` (`ORCHESTRATOR_URL`), authenticating with a
   `WORKER_TOKEN` passed as a query parameter, and reconnects forever with backoff.
4. Registers itself (hostname, OS, arch, Docker version, IP, runner version).
5. Enters an event loop: it **receives command messages** from the orchestrator, executes them
   against Docker, and **sends status/telemetry messages** back.

Alongside the command loop it runs several long-lived background goroutines: a heartbeat/metrics
ticker, a container log streamer, a network health monitor, a cron-style snapshot scheduler, an
exec-session janitor, and a local HTTP dashboard.

**What it owns:** the mechanics of talking to *one* Docker daemon on *one* host, and faithfully
translating orchestrator messages into Docker API calls and telemetry.

**What it does NOT own:** the desired state of the fleet, the stack/container/deployment data
model, deployment *policy* (which strategy, when, retries), auth issuance, or the UI. All of
that lives in the control plane — [`lattice-api`](https://github.com/aidenappl/lattice-runner)
(and its dashboard [`lattice-web`](https://github.com/aidenappl/lattice-web)). The runner never
initiates work on its own except for two things it is *told* to schedule: heartbeats and
cron snapshots.

## Stack & dependencies

- **Go 1.25** (`go.mod` declares `go 1.25.0`; builder image is `golang:1.25-alpine`). The README
  still says "Go 1.24+" for the install prerequisite — treat `go.mod` as authoritative.
- **`github.com/gorilla/websocket` v1.5.3** — the single persistent connection to `lattice-api`.
  The runner is a WebSocket *client*; it never listens.
- **`github.com/docker/docker` v27.5.1+incompatible** + **`github.com/docker/go-connections`** —
  the Docker Engine API SDK. All container/image/network/volume/exec operations go through this.
- **AWS SDK v2** (`aws-sdk-go-v2`, `.../credentials`, `.../service/s3`) — the S3 backup
  destination for database snapshots.
- **`google.golang.org/api` + `golang.org/x/oauth2`** — the Google Drive backup destination.
- Samba backups shell out to the system `smbclient` (see `backup/samba.go`); there is no Samba
  Go library dependency.
- Standard library everywhere else: `net`, `os/exec`, `crypto/sha256`, `syscall` (for
  `Statfs`/metrics), `runtime` (goroutine/heap metrics + panic stacks).

No web framework, no ORM, no external config library — config is plain `os.LookupEnv`.

## Project structure

Flat, package-per-concern layout at the repo root (consistent with the global Go standard: no
`cmd/`-as-entrypoint, `internal/`, or `pkg/`). `main.go` is the entrypoint and holds the entire
message dispatch `switch`.

| Path | Role |
|------|------|
| `main.go` | Entrypoint + **the message handler**. CLI dispatch (`setup`/`version`/default), Docker connect-with-retry, wires up every subsystem, and contains the ~40-case `switch env.Type` that handles every inbound message. Also holds `sendLifecycleLog`, `handleScheduledSnapshot`, the heartbeat loop, graceful shutdown, and `safeGo` (panic-recovering goroutine launcher). ~2,978 lines. |
| `validate.go` | `validContainerName` — the allow-list guard applied to every container name arriving from the orchestrator (alphanumeric + `-_./`, ≤128 chars). |
| `validate_test.go` | Table-driven tests for `validContainerName` (accepts normal names, rejects spaces and shell metacharacters `;$&\`|`). |
| `config/config.go` | `Config` struct + `Load()`. Reads env vars, **enforces `wss://`** unless `ALLOW_INSECURE=true`, panics on missing required vars. |
| `client/websocket.go` | The WebSocket client: `Envelope` (inbound) and `OutgoingMessage` (outbound) types, auto-reconnect loop, read/write pumps, ping/pong keepalive, buffered send channel with drop-on-full, `Drain`/`Close`. |
| `cmd/setup.go` | `RunSetup()` — interactive install wizard. Prompts for URL/token/name, writes `.env` (mode 0600), installs the `systemd` unit on Linux. |
| `deploy/executor.go` | `Executor`, `DeploymentSpec` + all nested spec types, `Validate()`, spec parsing, network/volume creation, stale-container cleanup, force-remove, `postDeployVerify`, strategy dispatch. |
| `deploy/rolling.go` | Rolling strategy (default). Suffixed-name deploy, health-gate, rename-to-canonical, rollback, orphan cleanup, and the **canonical-name lookup helpers** (`FindCanonicalContainer`, `isCanonicalVariant`). |
| `deploy/bluegreen.go` | Blue-green strategy: start "green" containers without host ports, health-check, then swap. |
| `deploy/canary.go` | Canary strategy: start one `-canary` container, monitor ~30s, then fall through to rolling if healthy. |
| `docker/docker.go` | The Docker Engine API wrapper (`Client`): container CRUD/lifecycle, image pulls with registry auth, networks, volumes, exec sessions, `GracefulRecreate`, `ContainerStats`, env-key/host-path validation. |
| `docker/logstreamer.go` | Per-container log streaming with `CanonicalContainerName` (strips deploy suffixes so logs map to the DB name). |
| `docker/netmonitor.go` | Network health monitor: detects DNS failures, bridge-only containers, restart loops; attempts auto-repair; reports via `lifecycle_log`. |
| `docker/database.go` | Database-container creation (`CreateDatabaseContainer`, `DatabaseSpec`) and dump/restore exec helpers (`ExecDatabaseDump`, `ExecDatabaseRestore`). |
| `docker/snapshot.go` | Snapshot helpers used by the db snapshot/restore flows. |
| `metrics/collector.go` | Host metrics from `/proc` (CPU, mem, swap, disk, net, load, uptime, processes) + `CollectRunnerMetrics` (goroutines/heap/sys). Linux-oriented. |
| `scheduler/scheduler.go` | Cron-style snapshot scheduler. Minute-aligned tick, custom 5-field cron matcher, per-instance in-flight dedupe. |
| `backup/destination.go` | `Destination` interface (`Upload`/`Download`/`Test`/`Delete`) + `NewDestination` factory keyed on `s3` / `google_drive` / `samba`. |
| `backup/s3.go`, `backup/gdrive.go`, `backup/samba.go` | The three backup-destination implementations. |
| `web/server.go`, `web/dashboard.go` | Local read-only status dashboard (HTTP). Binds `127.0.0.1:9100` by default. |
| `Dockerfile` | Multi-stage `golang:1.25-alpine` → `alpine:3.19`; ships `docker-cli` + `curl`; runs as UID 1001 in the `docker` group. |
| `Devfile.yaml` | `dev` CLI command wiring (`build`/`run`/`test`/`fmt`/`vet`/`check`/`tidy`). |

## Running, building & testing

This repo uses the standard `dev` CLI (`Devfile.yaml`):

```bash
dev build    # go build -o bin/app .
dev          # go run .            (needs ORCHESTRATOR_URL + WORKER_TOKEN in the env)
dev test     # go test ./...
dev fmt      # gofmt -w -s .
dev vet      # go vet ./...
dev check    # gofmt -w -s . && go vet ./... && go test ./...
dev tidy     # go mod tidy
```

The binary itself has three modes (dispatched in `main.go` from `os.Args[1]`):

```bash
lattice-runner           # default: start the daemon
lattice-runner setup     # interactive config + systemd install wizard (cmd/setup.go)
lattice-runner version   # print the Version string and exit
```

**Config prerequisites** (see the env table under *Domain & architecture*). At minimum
`ORCHESTRATOR_URL` and `WORKER_TOKEN` must be set or `config.Load()` panics on startup. A local
Docker daemon must be reachable or the runner exits after 30 failed connect attempts.

**How a runner is deployed to a node.** Not via this repo's Docker image in normal operation —
workers run the binary under `systemd` on the host so it can reach `/var/run/docker.sock` and
manage sibling containers. The canonical path is the one-liner from the Lattice dashboard
(*Workers → Add Worker* mints a `WORKER_TOKEN`):

```bash
curl -fsSL https://lattice-api.appleby.cloud/install/runner | WORKER_TOKEN=<token> WORKER_NAME=<name> bash
```

That script (served by `lattice-api`, not stored here) clones/builds, writes `/opt/lattice-runner/.env`
(mode 0600), installs the `systemd` unit, and starts the service. `lattice-runner setup` does the
same interactively. On macOS the wizard only writes a local `.env` (no systemd).

**Tests.** `go test ./...` covers: `validate_test.go` (root package), and per-package unit tests
in `config/`, `deploy/`, `docker/`, `backup/`, `scheduler/`. `client/`, `cmd/`, `metrics/`, and
`web/` have no test files. There is **no** integration-test harness here (no `go-trailblaze-fixtures`,
no Docker-backed suite) — that pattern is Trailblaze-specific and does not apply to this repo. Tests
are pure unit tests: container-name validation, cron matching, canonical-name stripping, backup
config parsing, deploy spec validation.

## How code is written here

The runner has one dominant pattern — the **message handler** — plus a set of house rules that
differ from a typical Go API.

- **One giant `switch env.Type` in `main.go`.** There is no router package and no per-message
  files. Every inbound message type is a `case` in the `ws.OnMessage(func(env client.Envelope){…})`
  switch. Adding a message = adding a `case`.
- **Every non-trivial handler runs in its own goroutine.** Container/deploy/db handlers do
  `handlerSem <- struct{}{}; go func(){ defer func(){ <-handlerSem }(); … }()`. `handlerSem` is a
  buffered channel of size 50 — a **global concurrency cap** so a burst of commands can't spawn
  unbounded goroutines. Lightweight/read handlers (`exec_*`, `list_*`, `reboot_os`) launch a bare
  `go func()` without the semaphore.
- **Panic isolation is deliberate and layered.** The whole `OnMessage` callback is wrapped in a
  `recover()` that logs the stack but keeps the WS read-pump alive (one bad message must not kill
  the process). Long-lived background goroutines are launched two ways depending on how fatal a
  panic is:
  - **`safeGo`** (the WS connect loop only): a panic is *unrecoverable* — the worker can't function
    without the socket — so it logs the stack, sends a **`worker_crash`** message, drains, and
    `os.Exit(2)` (so `systemd` restarts a cleanly-reported crash).
  - **`safeGoResilient`** (the telemetry/diagnostic loops: `log-streamer`, `net-monitor`,
    `heartbeat`): a panic degrades one subsystem but is **not** grounds to kill a worker that is
    otherwise running containers fine. It logs the stack, waits a 2s backoff, and **restarts the
    loop** — it does **not** emit `worker_crash` (that message means the worker is exiting) and does
    **not** `os.Exit`. It stops only when the loop returns normally (ctx cancelled on shutdown).
- **Container names from the orchestrator are always validated first.** Every lifecycle/db handler
  calls `validContainerName` (or `!validContainerName`) before touching Docker, and rejects with a
  `worker_action_status` error. This is the injection guard — names flow into Docker lookups and,
  for db dumps, into exec commands. `deploy/executor.go` has its own copy of the same function plus
  a `shellMetaChars` check on image refs.
- **Payloads are `map[string]any`; fields are type-asserted defensively.** JSON numbers arrive as
  `float64` and are converted (`int(v)`), strings via `x, _ := env.Payload["k"].(string)`. Missing
  fields degrade to zero values rather than erroring. The deploy spec is the exception: it is
  re-marshalled and `json.Unmarshal`-ed into the typed `DeploymentSpec` (`ParseDeploymentSpec`).
- **Every handler reports back.** Send an outbound status on both success and failure — `container_status`
  for lifecycle actions, `db_status` for db actions, `worker_action_status` for host/volume/network
  actions, plus streaming `lifecycle_log` lines for human-visible progress. Silence is a bug: the
  dashboard shows what the runner reports.
- **`wsSend`/`SendJSON` (best-effort) vs `wsSendReliable`/`SendJSONReliable` (blocking-with-deadline).**
  Both push onto the same 256-deep buffered send channel. `SendJSON` (via `wsSend`) does a
  **non-blocking** send and **drops the message when the queue is full** — correct for pure
  telemetry (heartbeat, metrics, container_sync, logs, lifecycle_log, deployment_progress), which is
  best-effort by design. `SendJSONReliable` (via `wsSendReliable`) **blocks until the queue has room
  or a 10s deadline elapses**, so it is used for the **command_id-correlated replies the orchestrator
  blocks waiting on** — `exec_output`, `list_volumes_response`, `list_networks_response`,
  `backup_dest_test_result`, `db_delete_snapshot_result`, and the `db_*_status` replies. A telemetry
  burst can no longer silently drop the one reply a caller is awaiting. It is still bounded (won't
  block forever if the socket is gone). **When adding a new correlated reply, send it via
  `wsSendReliable`, not `wsSend`.**
- **The recreate canonical-name fallback (a load-bearing convention).** Rolling deploys don't keep
  a container at its plain name during the swap — they create the new container as
  `<name>-<6charsuffix>`, health-check it, then rename to the canonical name. As a result a
  container may transiently exist under a *suffixed* name, or be left there if a rename failed.
  So the `recreate` handler, after a plain `FindContainerByName` miss, falls back to
  `executor.FindCanonicalContainer(ctx, name)`, which matches the exact name **or** any
  `isCanonicalVariant` — `<name>-ltc<6 lowercase-alnum>` (the **marker-prefixed** generated deploy
  suffix), `<name>-retired-*`, `<name>-lattice-updating`.
  The mirror of this is `docker.CanonicalContainerName`, which *strips* those same suffixes so log
  lines and heartbeat container-sync events are attributed to the DB name. **If you change the
  suffix format in one place, change it in all three** (`GenerateSuffix`,
  `docker.IsGeneratedSuffixSegment` used by `isCanonicalVariant`, and `CanonicalContainerName`) or
  containers become unfindable / logs orphan.
  - **Generated suffixes carry the `docker.SuffixMarker` (`"ltc"`) prefix** so the segment is
    `ltc` + 6 lowercase-alnum (e.g. `myapp-ltcz9i7q2`). This is deliberate: the previous *bare*
    6-char suffix collided with real container names ending in a 6-char word (`-worker`, `-server`,
    `-master`, `-canary`, `-backup`), which made canonical-name stripping and `recreate` target the
    **wrong** container. The matchers now **never** strip a bare 6-char segment — only the
    marker-prefixed suffix, `-retired-*`, and `-lattice-updating`.
  - **Transition note:** containers deployed by an *older* runner during the upgrade window may
    carry the old bare-6-char suffix; the new matchers will NOT recognize those as variants. In
    practice this self-heals — such names are transient (renamed to canonical on deploy success) or
    get cleaned as orphans on the next deploy — and correctness (not targeting the wrong container)
    was preferred over recognizing the ambiguous old format.
- **Deploy progress uses a state map + retry-aware callback.** `deploymentStates[deploymentID]`
  tracks status/step/attempt across the async deploy; the executor's `ProgressCallback` enriches
  every progress event with `attempt`/`max_retries`/`last_progress_at` and emits
  `deployment_progress`. Old states are GC'd from the heartbeat loop after 15 min idle.
- Naming follows global Go conventions elsewhere (`getEnv`/`getEnvOrPanic` in `config`, PascalCase
  exported types). There is **no** responder/query/struct package split — this is a daemon, not an
  HTTP API, so those standards don't apply. The one HTTP surface (`web/`) is a plain `net/http` mux.

## Domain & architecture

### Connection model — outbound only

The runner is a **WebSocket client**. It dials `ORCHESTRATOR_URL` (e.g.
`wss://lattice-api.appleby.cloud/ws/worker`) with the worker token appended as a `?token=…` query
parameter (`client/websocket.go` `dial`). This is the entire security model of the link: **there
are no inbound ports** on the worker (the only listener is the localhost-bound dashboard). A
worker behind NAT or a restrictive firewall works with zero inbound rules — it only needs outbound
443. `config.Load` refuses a `ws://` URL unless `ALLOW_INSECURE=true`, because the token and every
payload (registry passwords, env vars, db credentials) travel over this socket in the clear
otherwise.

Keepalive/liveness (`client/websocket.go`): the write pump sends a WS **ping every 54s**; the read
pump sets a **60s read deadline** refreshed by each pong. Read limit is **4 MB** (deploy specs can
be large). On any read/write error the connection tears down and `Connect` reconnects after
`RECONNECT_INTERVAL` (default 5s), looping forever until `ctx` is cancelled. The outbound `send`
channel is buffered at **256**; a full channel drops the message and logs it (telemetry is
best-effort).

### Message protocol (the contract with `lattice-api`)

Messages are JSON. **Inbound** = `client.Envelope` `{ type, command_id?, worker_id?, issued_at?, payload }`.
**Outbound** = `client.OutgoingMessage` `{ type, command_id?, status?, payload }`. `command_id`
correlates a request with its responses (used by exec, volume/network list, and backup-test flows).

**Inbound messages the runner handles** (every `case` in `main.go`'s switch). Unlisted types are
silently ignored.

| Inbound `type` | Payload (key fields) | What the runner does | Outbound replies |
|---|---|---|---|
| `connected` | — | Orchestrator acked the socket; runner sends its registration | `registration` |
| `deploy` | full `DeploymentSpec` (`deployment_id`, `stack_name`, `strategy`, `targeted`, `force`, `containers[]`, `networks[]`, `volumes[]`, `attempt`, `max_retries`) | Parse+validate spec, create nets/volumes, run strategy, verify | `deployment_progress` (repeated) |
| `deployment_ping` | `deployment_id` | Report current in-memory deploy state | `deployment_status` |
| `start` | `container_name` | Find + start container | `container_status`, `lifecycle_log` |
| `stop` | `container_name` | Find + stop (30s timeout) | `container_status`, `lifecycle_log` |
| `restart` | `container_name` | Find + restart (30s) | `container_status`, `lifecycle_log` |
| `kill` | `container_name` | Find + SIGKILL | `container_status`, `lifecycle_log` |
| `pause` | `container_name` | Pause | `container_status`, `lifecycle_log` |
| `unpause` | `container_name` | Unpause | `container_status`, `lifecycle_log` |
| `remove` | `container_name` | Stop (10s) then force-remove | `container_status`, `lifecycle_log` |
| `recreate` | `container_name`, `image?`, `tag?`, `auth?` | Pull image (best-effort), find via **canonical fallback**, `GracefulRecreate` | `container_status`, `lifecycle_log` |
| `pull_image` | `image`, `auth?` | Pull image with optional registry auth | `container_status`, `lifecycle_log` |
| `force_remove` | `container_name` | Stop (5s) + force-remove | `worker_action_status` |
| `reboot_os` | — | `sudo reboot` (5-min cooldown enforced) | `worker_action_status` |
| `upgrade_runner` | `expected_hash` (**required**) | Download install script from derived URL, **verify SHA-256** against `expected_hash`, run it. **Fails closed:** an empty/missing `expected_hash` aborts the upgrade before any download/execute (no unverified script is ever run) | `worker_action_status` |
| `stop_all` | — | Stop every running container | `worker_action_status`, `lifecycle_log` |
| `start_all` | — | Start every non-running container | `worker_action_status`, `lifecycle_log` |
| `list_volumes` | — | List Docker volumes | `list_volumes_response` |
| `create_volume` | `name`, `driver?` | Create a volume (`local` default) | `worker_action_status` |
| `remove_volume` | `name`, `force?` | Remove a volume | `worker_action_status` |
| `list_networks` | — | List Docker networks | `list_networks_response` |
| `create_network` | `name`, `driver?` | Create a network (`bridge` default) | `worker_action_status` |
| `remove_network` | `name` | Remove a network | `worker_action_status` |
| `exec_start` | `container_name`, `cmd?` (uses `command_id`) | Create+attach an exec session (`/bin/sh` default), stream output | `exec_output` (streamed, base64) |
| `exec_input` | `data` (b64), `command_id` | Write stdin to the exec session | — |
| `exec_resize` | `height`, `width`, `command_id` | Resize the exec TTY | — |
| `exec_close` | `command_id` | Cancel/close the exec session | `exec_output` (`closed:true`) |
| `db_create` | `container_name`, `engine`, `engine_version`, `port`, creds, `volume_name?`, limits (`memory_limit` in **bytes**) | Ack → probe the host port → pull → create+start a managed database container | `db_status` (`ack`→`completed`/`failed`), `lifecycle_log` |
| `db_start` / `db_stop` / `db_restart` | `container_name` | Lifecycle for a db container | `db_status`, `lifecycle_log` |
| `db_remove` | `container_name`, `remove_volume?`, `volume_name?` | Destroy a db container. Preserves the data volume unless `remove_volume` is set, which purges `volume_name` too (that is how a delete differs from the `remove` action). **Idempotent** — an absent container is success, not failure | `db_status` (with `volume_removed`), `lifecycle_log` |
| `db_snapshot` | `container_name`, `engine`, `database_name`, creds, `snapshot_id`, `remote_path`, `dest_type`, `dest_config` | Dump db → temp file → upload to backup destination | `db_snapshot_status` (`uploading`→`completed`/`failed`), `lifecycle_log` |
| `db_restore` | as snapshot + `restore_id` | Download from destination → restore into db | `db_restore_status` (`downloading`→`completed`/`failed`), `lifecycle_log` |
| `db_update_schedule` | `instance_id`, `enabled`, `container_name`, `engine`, creds, `cron`, `retention_count`, `backup_dest` | Add/update or remove a scheduled snapshot job | `db_schedule_status` |
| `backup_dest_test` | `dest_type`, `dest_config` | Build destination + connectivity `Test()` | `backup_dest_test_result` |
| `db_delete_snapshot_file` | `dest_type`, `dest_config`, `remote_path`, `snapshot_id` | Delete a snapshot file from the destination | `db_delete_snapshot_result` |
| `db_sync_request` | — | Report every `lattice-type=database` container it can see | `db_sync` |

**Outbound messages the runner sends.** Every one is a `type` on `OutgoingMessage`. Every `db_*`
reply carries the correlation fields echoed by `sendDbReply` — see *Managed databases* below;
omitting `database_instance_id` makes the reply unusable to the orchestrator.

| Outbound `type` | When | Notable payload |
|---|---|---|
| `registration` | After `connected` | `name`, `hostname`, `os`, `arch`, `docker_version`, `ip_address`, `runner_version` |
| `heartbeat` | Every `HEARTBEAT_INTERVAL` | full host metrics + `runner_version`/goroutines/heap; `container_stats` every 3rd tick |
| `container_sync` | Each heartbeat, per container | `container_name` (canonical), `state`, mapped `status`, optional `health_status` — DB reconciliation |
| `container_health_status` | Heartbeat, when Docker reports a health string | `container_name`, `health_status` (`healthy`/`unhealthy`/`starting`) |
| `container_status` | End of a lifecycle action | `container_name`, `action`, `status` (`success`/`failed`), `message?` |
| `worker_action_status` | Host/volume/network/force-remove/reboot/upgrade/stop_all/start_all | `action`, `status`, `message` |
| `lifecycle_log` | During actions + network diagnostics | `container_name`, `event`, `message` — streamed human-readable progress |
| `deployment_progress` | Throughout a deploy | `deployment_id`, `status`, `step`, `message`, `attempt`, `max_retries`, `last_progress_at` |
| `deployment_status` | Reply to `deployment_ping` | current `status`/`step`/`in_progress` for a deployment |
| `container_logs` | Continuously (log streamer) | `container_name`, `stream`, `message`, `recorded_at` (RFC3339Nano) |
| `exec_output` | During an exec session | `command_id`, `data` (b64) / `error` / `closed` |
| `list_volumes_response` / `list_networks_response` | Reply to the matching list command | `command_id`, `status`, `volumes`/`networks` |
| `db_status` | On receipt of, and at the end of, a db lifecycle action | `database_instance_id`, `request_id`, `idempotency_key`, `container_name`, `action`, `phase` (`ack`/`completed`/`failed`), `status`, `container_id?` |
| `db_snapshot_status` / `db_restore_status` | Snapshot/restore progress | ids, `status`, `size_bytes?`, `error_message?`, `scheduled?` |
| `db_schedule_status` | Reply to `db_update_schedule` | `instance_id`, `status` (`updated`/`removed`), `cron?` |
| `backup_dest_test_result` | Reply to `backup_dest_test` | `command_id`, `status`, `message` |
| `db_delete_snapshot_result` | Reply to `db_delete_snapshot_file` | `snapshot_id`, `status`, `message?` |
| `db_sync` | Every 60s and on `db_sync_request` | `containers[]` of `container_name`, `container_id`, `state`, `health`, `restart_count`, `fatal_hint?` — the observed state the orchestrator reconciles against |
| `worker_shutdown` | Graceful shutdown | `reason`, `message` — lets the orchestrator mark the worker offline cleanly |
| `worker_crash` | A `safeGo` goroutine panicked | `goroutine`, `panic`, `stack` — sent just before `os.Exit(2)` |

### Deploy execution & strategies

`lattice-api` decides *what* and *which strategy*; the runner *executes*. A `deploy` message
carries a `DeploymentSpec`. `Executor.Execute` (in `deploy/executor.go`):

1. `Validate()` — caps (≤100 containers, ≤50 networks, ≤50 volumes), valid names, non-empty
   images, and rejects image refs containing shell metacharacters.
2. Pre-flight disk check (≥1 GB free on `/`).
3. Create declared networks (default driver `bridge`) and volumes (default `local`) — idempotently.
4. Unless `targeted`, `cleanupStaleContainers` — remove `managed-by=lattice` containers of this
   stack that are no longer in the spec (renamed/removed compose services), plus a port-conflict
   fallback for unlabeled containers. `targeted` deploys skip this (the spec is only a subset).
5. If `force`, `forceRemoveAllStackContainers` — wipe *all* of the stack's containers for a clean
   slate (used to recover from failed deploys).
6. Dispatch on `spec.Strategy` (default → **rolling**):
   - **rolling** (`rolling.go`, default): per container, pull the new image, create it under a
     `<name>-<6charsuffix>` temp name, health-gate it, then stop/remove the old and rename the new
     to the canonical name. Keeps snapshots to `rollbackContainers` on failure; cleans up orphans.
   - **blue-green** (`bluegreen.go`): start all "green" containers *without* host port bindings
     **and without the canonical `NetworkAliases`** (blue is still live under those aliases — sharing
     them would make Docker's embedded DNS round-robin the service name onto the not-yet-ready green),
     health-check by container ID, then swap ports + canonical aliases over to green and retire the
     old ("blue").
   - **canary** (`canary.go`): start one `<name>-canary`, monitor for ~30s; if healthy, proceed as
     a rolling deploy; otherwise abort.
7. `postDeployVerify` — 6 checks at 10s intervals (60s). Flags containers that vanished, are stuck
   `restarting`, `exited`/`dead`, have a non-zero `RestartCount`, or report `unhealthy`. Persistent
   issues at the final check fail the deployment.

Progress is streamed as `deployment_progress` throughout; `main.go` layers retry bookkeeping
(`attempt`/`max_retries`) on top. The **in-flight guard is atomic (single-lock check-and-set)** and
serializes **by stack name**: a duplicate of the same `deployment_id` is rejected while it is
`deploying` OR in the ~60s `validating` window (tracked by an `InProgress` flag, not just
`Status=="deploying"`), and a *different* `deployment_id` targeting a stack that already has an
in-flight deployment is rejected with a `deployment_progress{status:"failed"}` reply.

**Partial-failure safety in the strategies** (all three keep the previous container recoverable
until the replacement is verified):
- **rolling** — on a create/verify failure *after* the predecessor was stopped, the in-flight
  container's index is added to the rollback set so its just-stopped predecessor is restored, not
  left down. A snapshot with no predecessor (`OldID==""`) is *removed* on rollback rather than
  recreated with the failing image.
- **blue-green** — the old ("blue") container is **renamed to a retired name and kept stopped** (not
  removed) during the swap; the final container is health-verified before blue is removed, and a
  swap failure rolls back by renaming blue back and starting it.
- **`GracefulRecreate`** (single-container `recreate`) — for a port-bound container, the final
  (real-port) container is **health-gated after start**, and the old container is kept renamed+stopped
  until the final is verified; failure restores the old container.

### Docker lifecycle management

`docker/docker.go` wraps the Engine SDK. Containers are created with `managed-by=lattice` and
`lattice-stack=<name>` labels (how cleanup identifies ownership). Registry auth for private pulls
is passed per-deploy/per-recreate and base64-encoded into the Docker pull auth header. Host-path
mounts and env keys are validated (`validateHostPath`, `validateEnvKey`). `GracefulRecreate`
inspects the live container, recreates it (optionally with a new image) under a
`-lattice-updating` temp name *without host port bindings*, preserves its network config, health
checks, then swaps — the low-conflict path used by the single-container `recreate` command.

### Metrics reporting

`metrics/Collect` reads host stats from `/proc` and `syscall.Statfs` (CPU delta from `/proc/stat`,
mem/swap from `/proc/meminfo`, disk from `Statfs("/")`, network from `/proc/net/dev` summing
physical interfaces and excluding `docker*`/`br-*`/`veth*`/`virbr*`/`lo`, load/uptime/processes
from `/proc`). `CollectRunnerMetrics` adds the runner's own goroutine count and heap/sys memory.
Both are emitted every `HEARTBEAT_INTERVAL` in the `heartbeat` message; per-container resource
stats (`ContainerStats`) are attached only every 3rd heartbeat because they're expensive. The same
heartbeat tick also pushes a `container_sync` snapshot per container so the orchestrator's DB
stays reconciled even when containers change state outside Lattice.

### Managed databases

`docker/database.go` creates and inspects managed database containers, labelled
`managed-by=lattice`, `lattice-type=database`, `lattice-engine=<engine>`. Selection is always by
label, never by name prefix, so a container that merely looks like a database is never treated as
one.

**Every `db_*` reply must go through `sendDbReply` (`main.go`).** It echoes
`database_instance_id`, `request_id` and `idempotency_key` from the triggering command and derives
a `phase` (`ack` → `completed`/`failed`). This is not optional bookkeeping: replies were originally
built as bare payload literals with no instance ID, so the orchestrator could not match a reply to
the row it was meant to update and **no managed database could ever leave `pending`**, whether the
operation succeeded or failed. `buildDbReplyPayload` holds the logic separately so it is testable
without a socket. Scheduler-triggered replies have no incoming command, so they use
`scheduledEnv(instanceID)` to correlate.

**`database_observer.go`** reports observed state so the orchestrator can reconcile against
reality rather than trusting that command replies arrived:

- `startDatabaseObserver` sends `db_sync` every 60s, immediately on start, and on demand in
  response to `db_sync_request`.
- Each entry carries Docker state, mapped health, and restart count. A container with ≥3 restarts
  is additionally scanned for a known-fatal startup signature (`fatalInitSignatures` — wrong volume
  ownership, a non-empty Postgres data directory, and so on) and the diagnosis is attached as
  `fatal_hint`, so the orchestrator can say *why* rather than "it keeps restarting".
- `probeHostPort` binds the host port for real before `db_create` pulls the image. Probing is not a
  substitute for the orchestrator's ledger — a port can be taken between check and bind — but it
  turns a late, opaque Docker error into an immediate, specific one.
- `normaliseMemoryLimit` converts a limit below Docker's 6MB floor from megabytes to bytes. Such a
  value is never legitimate; it always means the caller skipped the conversion, which is exactly
  the bug that made every create fail when 512MB arrived as 512 bytes.

**Data volumes are never silently reused.** `CreateDatabaseContainer` refuses to start when
`lattice-dbdata-<name>` already exists unless `adopt_existing_volume` is set. Every official
database image only initialises when its data directory is empty, so attaching a populated volume
means `MARIADB_USER`/`POSTGRES_PASSWORD` and friends are ignored entirely: the container starts,
reports healthy, and serves its *previous* credentials while the control plane records the new ones
it just generated. Nothing looks wrong until a connection fails. `db_remove` deliberately preserves
volumes unless the orchestrator asks for a purge, so this is reachable whenever a database is
recreated under a previously-used name.

**`db_remove` is idempotent in both halves.** An already-absent container is the *goal* of a remove,
so it is reported as success with the volume phase still carried out — replying `failed: container
not found` (as it once did) drove the control plane's instance into `error` on any second remove and
left a purge unable to reach the volume the first one stranded. The volume removal passes
`force=true` for the same reason: the daemon then treats an already-absent volume as success, so a
retried delete converges instead of failing on the volume it deleted last time. A purge that cannot
remove the volume fails loudly rather than reporting success — the control plane retires the database
record only on `volume_removed: true`.

Database healthchecks use a **60s `start_period`**. Failures inside it don't count toward the
failing streak, which matters because a cold database legitimately takes far longer than a moment
to initialise — first-time `initdb`, or InnoDB crash recovery on restart.

### Scheduler (cron snapshots)

`scheduler/scheduler.go` keeps a map of `Job`s keyed by database instance ID, set via
`db_update_schedule`. `Run` re-aligns to the next minute boundary each iteration (no drift) and
fires jobs whose 5-field cron expression matches. It supports `*`, exact values, comma lists,
ranges (`1-5`), and steps (`*/5`, `1-30/5`) via a hand-written matcher (no cron library). A
per-instance `inflight` `sync.Map` skips a fire if the previous snapshot for that instance is still
running. Fired jobs call back into `main.go`'s `handleScheduledSnapshot`, which mirrors the
`db_snapshot` flow and reports `db_snapshot_status` with `scheduled:true`, correlated via
`scheduledEnv`.

### Backup subsystem

`backup/` abstracts snapshot storage behind the `Destination` interface
(`Upload`/`Download`/`Test`/`Delete`). `NewDestination(type, config)` builds one from a type string:
`s3` (AWS SDK v2), `google_drive` (Google API + OAuth2), or `samba` (shells to `smbclient`). The
db snapshot/restore handlers and the scheduler all go through this factory; `dest_config` is an
opaque `map[string]any` forwarded straight from the orchestrator.

### Worker identity & tokens

A worker's identity is its `WORKER_TOKEN` (minted by `lattice-api` when the worker is created) plus
the `registration` it sends on connect. There is no local persistent identity beyond the token in
`.env`. A revoked/rotated token means the WS handshake is rejected and the runner reconnect-loops;
the fix is a new token, not a code change. `WORKER_NAME` is cosmetic (defaults to hostname).

### Configuration (env surface)

Read in `config/config.go` (unless noted). Required vars **panic** if missing.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `ORCHESTRATOR_URL` | **Yes** | — | WebSocket URL to `lattice-api` (`wss://…/ws/worker`). `ws://` rejected unless `ALLOW_INSECURE`. |
| `WORKER_TOKEN` | **Yes** | — | Worker auth token; sent as `?token=` on the WS handshake. |
| `WORKER_NAME` | No | hostname | Human-readable worker name in registration. |
| `HEARTBEAT_INTERVAL` | No | `10s` | Metrics/heartbeat cadence. (README says 15s; code default is 10s.) |
| `RECONNECT_INTERVAL` | No | `5s` | WS reconnect backoff. |
| `DASHBOARD_PORT` | No | `9100` | Local dashboard HTTP port. |
| `LATTICE_URL` | No | `""` | Link back to the orchestrator UI, shown on the dashboard. |
| `ALLOW_INSECURE` | No | `false` | Set `true` to permit an unencrypted `ws://` URL (local dev only). |
| `DASHBOARD_BIND` | No | `127.0.0.1` | Dashboard bind address (read in `web/server.go`). Localhost-only by default. |

### Local dashboard

`web/` serves a read-only status UI, bound to `127.0.0.1:9100` by default (so it isn't exposed on
the worker's public interface unless `DASHBOARD_BIND` is widened). Routes: `GET /` (HTML dashboard),
`GET /api/status` (host metrics JSON), `GET /api/containers` (container list JSON),
`GET /api/containers/{id}/logs?tail=N` (logs). It is a diagnostic convenience, not part of the
control-plane protocol.

### Graceful shutdown

On `SIGINT`/`SIGTERM`: wait up to 60s for in-flight deployments to finish, send `worker_shutdown`,
pause ~2s for in-flight handlers to enqueue their final status, `Drain` the send queue (5s) **while
the write pump is still alive**, then cancel the root context (stopping all goroutines) and `Close`
the socket. **Order matters: `Drain` must happen BEFORE `cancel()`** — the write pump exits on
context cancellation, so cancelling first would strand every queued message (including
`worker_shutdown`).

## Ecosystem & related repos

| Repo | Relationship |
|------|--------------|
| [`lattice-api`](https://github.com/aidenappl/lattice-api) | **The control plane.** Hosts the `/ws/worker` endpoint this runner dials, issues every command message, receives all telemetry, mints worker tokens, serves the install/upgrade scripts, and owns the data model. The message protocol above is a shared contract with this repo — the server side lives here. |
| [`lattice-web`](https://github.com/aidenappl/lattice-web) | Next.js dashboard over `lattice-api`. Where an operator adds a worker, triggers deploys/lifecycle actions, and watches the `lifecycle_log`/`deployment_progress`/`container_logs` streams this runner emits. |
| [`lattice-mcp`](https://github.com/aidenappl/lattice-mcp) | MCP server exposing the `lattice-api` admin surface to Claude Code as typed tools (`lattice_reboot_worker`, `lattice_upgrade_worker`, `lattice_recreate_container`, …). Those tools ultimately cause the messages this runner handles. |

The runner has **no** direct dependency on any `appleby.cloud` infra service other than the Docker
daemon on its host and the WebSocket to `lattice-api`. It does not import `go-forta`, `go-keyring`,
or `go-monitor`.

## Operations

- **Where it runs:** one instance per worker VM, as a `systemd` service
  (`/etc/systemd/system/lattice-runner.service`, `Restart=always`, `RestartSec=5`), working dir
  `/opt/lattice-runner`, config in `/opt/lattice-runner/.env` (mode 0600). It needs access to
  `/var/run/docker.sock`.
- **Logs/metrics:** `sudo journalctl -u lattice-runner -f` on the host; centrally, the worker's
  heartbeats/lifecycle logs/deploy progress show up in `lattice-web` and via the Lattice MCP
  (`mcp__lattice__lattice_get_worker`, `lattice_get_container_logs`, `lattice_get_deployment_logs`,
  `lattice_get_anomalies`). The local dashboard (`http://127.0.0.1:9100`) is a last-resort on-host view.
- **How it's updated:** the control plane drives upgrades. An `upgrade_runner` message (fleet-wide
  or per-worker; `lattice-api` exposes worker-upgrade tooling, surfaced in the MCP as
  `lattice_upgrade_worker`) tells the runner to download the install script from
  `<orchestrator-http-base>/install/runner`, **verify its SHA-256** against `expected_hash`, run it,
  and let `systemd` restart the new binary. A `reboot_os` message triggers `sudo reboot` (rate-limited
  to once per 5 min). Manual update: `curl -fsSL https://lattice-api.appleby.cloud/install/update.sh | bash`.
- **Common failure modes:**
  - *Worker shows offline / reconnect loop* — bad or revoked `WORKER_TOKEN`, wrong `ORCHESTRATOR_URL`,
    or the TLS proxy in front of `lattice-api` has an expired cert. The runner keeps retrying every
    `RECONNECT_INTERVAL`; check journald for `ws: connection failed`.
  - *Runner won't start* — `config.Load` panicked on a missing `ORCHESTRATOR_URL`/`WORKER_TOKEN`, or
    it exited after 30 failed Docker connects (`docker.sock` perms / daemon down).
  - *`systemd` restart loop* — the WS connect loop panicked (the only loop that exits on panic);
    look for a `worker_crash` message (it carries the goroutine name + stack) and the corresponding
    journald `[ws-connect] PANIC` line. A panic in a telemetry loop (`log-streamer`/`net-monitor`/
    `heartbeat`) does **not** restart the process — it self-heals via `safeGoResilient` and shows up
    only as a journald `[<name>] PANIC (recovered, restarting loop…)` line, so a subsystem going
    quiet without a restart is the signature there.
  - *Deploy stuck/failed* — inspect `deployment_progress`/`lifecycle_log`; `postDeployVerify` fails a
    deploy when containers crash-loop or report unhealthy within 60s.
  - *Container "not found" on a lifecycle action after a deploy* — likely still under a suffixed
    name; `recreate` handles this via the canonical fallback, but plain start/stop/restart use exact
    lookup only.
  - *Dropped telemetry* — the 256-deep send queue overflowed under a burst (`ws: send queue full`);
    pure telemetry is best-effort by design. Command_id-correlated replies use `SendJSONReliable`
    (blocking with a 10s deadline) so they are not dropped on a transient full queue; a
    `ws: reliable send timed out` log means the socket was genuinely stuck for 10s.

## Rules & guardrails

- **Never add an inbound listener to the worker.** The outbound-only WebSocket is the security
  model — a worker needs zero inbound firewall rules. The only listener is the localhost-bound
  dashboard; do not bind it publicly by default and do not add a second server.
- **Never break the message protocol contract.** Renaming an inbound `type`, dropping an outbound
  message, or changing a payload field breaks `lattice-api`'s side and the whole worker fleet. If
  you must evolve it, coordinate the change in `lattice-api` in lockstep and update the tables above
  in the same change. Read the `lattice-api` handler — don't infer the shape.
- **Always validate container names from the orchestrator** with `validContainerName` before any
  Docker call or exec. Names reach shell (db dumps) and Docker lookups.
- **Keep the suffix helpers in sync** — `GenerateSuffix`, `docker.IsGeneratedSuffixSegment` (used by
  `isCanonicalVariant`), and `CanonicalContainerName`. They encode one format
  (`<name>-ltc<6 lowercase-alnum>` via `docker.SuffixMarker`, `-retired-*`, `-lattice-updating`);
  divergence orphans containers and logs. **Never** match/strip a bare 6-char segment — it collides
  with real names (`-worker`/`-server`/`-master`/`-canary`/`-backup`) and targets the wrong container.
- **Preserve panic isolation.** Handlers must not be allowed to crash the read pump; long-lived
  goroutines go through `safeGo` so a panic is reported as `worker_crash` before exit.
- **Don't log secrets.** Deploy specs, `recreate`/`pull_image` `auth`, and db handlers carry
  registry passwords and database credentials. Log names and outcomes, not payloads.
- **Every handler must reply.** Emit a `*_status`/`worker_action_status` on both success and
  failure so the dashboard reflects reality.
- **Respect the global guardrails:** never push/deploy without explicit instruction, never modify
  `Dockerfile`/CI/compose unless asked, never create/modify `.env`.
- **`upgrade_runner` must keep verifying the script hash** — dropping the SHA-256 check turns a
  worker into a remote-code-execution target. It **fails closed**: a missing/empty `expected_hash`
  aborts the upgrade (never runs an unverified script). Don't reintroduce a "warn and run anyway" path.
- **Validate orchestrator-supplied Samba `remote_path`** (`backup/samba.go` `validateSMBPath`) before
  building any `smbclient -c` command — `;`, newlines, backticks, `$`, `"`, and backslashes are
  rejected to prevent smbclient/command injection.

## Verification — always before "done"

```bash
gofmt -w -s .        # format (CI rejects unformatted Go)
go build ./...       # must compile
go test ./...        # unit tests: root (validate, db observer), config, deploy, docker, backup, scheduler
go vet ./...         # must be clean
```

`dev check` runs fmt + vet + test in one shot. **CI runs all of the above in a `test` job that
`build-and-push` depends on**, so unformatted code or a failing test blocks the image build and the
deploy. The formatting gate uses plain `gofmt -l .`.

There is no integration suite and no live smoke test in this repo. If you change the message
protocol, the real verification is a round-trip against a running `lattice-api` (or its worker WS
endpoint) — schema-level agreement is not enough, and the database subsystem is the cautionary
example: eight separate defects shipped because both sides compiled fine and nobody exercised the
contract end to end.

> The `gofmt` drift this file previously warned about is gone — the tree is clean under both
> `gofmt -l .` and `gofmt -l -s .`. Still format only what your change touches rather than sweeping
> unrelated files.

## Keeping this file updated

Update this AGENTS.md in the same change when you:
- **Add/remove/rename an inbound `type` or outbound message** → update the protocol tables *and*
  coordinate with `lattice-api`.
- **Change a payload field, the `DeploymentSpec` shape, or a deploy strategy** → update
  *Domain & architecture*.
- **Add/remove an env var or change a default** → update the config table (and `README.md` /
  `config/config.go` if the default moved).
- **Change the Docker interaction model, canonical-suffix format, or panic/shutdown behavior** →
  update *How code is written here*.
- **Add a package** → add a row to *Project structure*.
- **Change how a runner is deployed or upgraded** → update *Running…* and *Operations*, and
  `README.md` if the install path changed.
