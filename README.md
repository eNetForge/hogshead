# hogshead

A small, single-binary, S3-compatible object store for homelabs, CI runners and
small teams. Runs with one command, stores on local disk, works unchanged with
existing S3 clients. Buckets can be replicated to one or more standby servers
for an off-box copy.

> Status: pre-release. The S3 API, users and policies, lifecycle, TLS, the
> admin API and CLI, the web UI and bucket replication are implemented and tested.
> The client matrix (aws cli v2, s3cmd, rclone, restic, boto3) passes and 295
> s3-tests conformance cases are green in CI; no release has been cut yet. See
> [TASKS.md](TASKS.md) for what remains and `test/REPORT.md` for known gaps.

Website and documentation: <https://hogshead.io>

## Run it

With Docker (the first start prints the admin credentials to the log):

```sh
docker run -d --name hogshead -p 9000:9000 -v hogshead:/data ghcr.io/enetforge/hogshead
docker logs hogshead
```

With Docker Compose: copy [compose.yaml](compose.yaml), then `docker compose up -d`.

As a binary:

```sh
curl -fsSL https://github.com/eNetForge/hogshead/releases/latest/download/hogshead_linux_amd64.tar.gz | tar xz hogshead
sudo install hogshead /usr/local/bin/
hogshead quickstart     # writes the config, prints the admin credentials
hogshead serve
```

From source: `make ui build` (Go 1.26 and Node 22), or `docker build -t hogshead .`.

The S3 endpoint is `http://localhost:9000`; the web UI is at `/_ui/`.

In a container the config file, data and admin socket live in `/data`, and the
command line works through `docker exec hogshead hogshead <command>`.
`serve --init` (the image's default command) performs the `quickstart` setup
when `/data/config.toml` does not exist yet. Set `HOGSHEAD_INIT_ACCESS_KEY`,
`HOGSHEAD_INIT_SECRET_KEY` and `HOGSHEAD_INIT_PASSWORD` to choose the admin
credentials instead of having them generated; they are read on the first start
only.

## Why

Self-hosted object storage tends to be either a distributed system that needs
a team to run, or a test double that real clients outgrow. hogshead aims at
the space between: trivial to run on one box, correct enough that `aws`,
`rclone`, `restic` and `Velero` just work, and able to grow to a few servers
that hold copies of each other's data without a distributed-systems degree.

## Goals

- **One binary, one command.** `hogshead quickstart` gives you a running
  server, an admin key, a config file and a systemd unit.
- **Boringly compatible.** Passes the `s3-tests` conformance subset for the features
  we claim, and the real-world client matrix in CI.
- **Local disk first.** No object gateway in front of another object store.
- **Several servers without a cluster to operate.** Group servers into a
  cellar and every member keeps a full copy of the objects, users and
  settings, and accepts writes. Or replicate chosen buckets to a standby.
  Either way each server stays a complete, independent server: no Raft, no
  quorum, no cluster port.
- **Everything through the API.** Every admin operation exists in the admin
  JSON API first. The CLI and the web UI are both clients of it. Nothing is
  UI-only or CLI-only.
- **No restarts to reconfigure.** Every setting applies live. The only
  exceptions are the listen addresses, the data directory, the TLS mode, the
  encryption key file and the admin socket.

## Non-goals

- Splitting data across machines. Servers can work together (a cellar, or
  bucket replication), but always by each holding a full copy. There is no
  erasure coding, no sharding, no pooling of disks across machines and no
  shared metadata that needs a quorum.
- Redirecting clients when a server fails. Every cellar member can serve
  every request, so failover is a load balancer or DNS in front of them.
- Full IAM policy language fidelity. We implement a documented subset.
- Glacier-style storage classes or tiering.
- Compatibility with other servers' admin APIs. The S3 API is the contract;
  administration has its own JSON API.
- Alternative metadata backends (Postgres). SQLite plus bucket replication
  covers the use this is built for.

## Features (target for 1.0)

**S3 API**
- Buckets: create, list, delete, head, location.
- Objects: PUT, GET, HEAD, DELETE, DeleteObjects, CopyObject, range GET,
  conditional headers (`If-Match`, `If-None-Match`, `If-Modified-Since`,
  `If-Unmodified-Since`), user metadata, content-type.
- ListObjectsV2 (and V1 for old clients) with prefix, delimiter, pagination.
- Multipart: initiate, upload part, upload part copy, complete, abort, list
  parts, list multipart uploads.
- Versioning per bucket with delete markers, ListObjectVersions.
- Checksums: MD5 ETag semantics matching AWS (including multipart `-N`
  ETags), SHA256 and CRC32C trailers and `x-amz-checksum-*` headers.
- Lifecycle: expiration by age, noncurrent version expiry, abort incomplete
  multipart.
- Bucket policy subset: public read, per-principal allow/deny by bucket and
  prefix, condition-free.
- CORS configuration per bucket.
- Presigned URLs (SigV4 query auth) for GET and PUT.

**Auth and security**
- SigV4 header and query auth, path-style and virtual-host-style addressing,
  chunked (`aws-chunked`) uploads.
- Users with access keys. Roles: `admin`, `read-write`, `read-only`,
  optionally scoped to a bucket list.
- No anonymous access unless a bucket policy grants it.
- Constant-time signature comparison, failed-auth rate limiting per source IP.
- Optional encryption at rest with a local master key (AES-256-GCM per chunk).
- Audit log of admin and auth events.
- TLS via ACME (HTTP-01 and DNS-01) or a supplied certificate.

**Operations**
- Web UI: sign in with a user name and password (or an access key), browse,
  drag-and-drop upload (files and folders), preview images, media, PDFs and
  text, edit text objects inline, download, share presigned links, manage
  buckets, users, keys, passwords and replication targets, see server status.
- Event notifications to webhooks: `PutBucketNotification` with
  `arn:hogshead:webhook:::<name>` destinations, URLs kept in server settings,
  HMAC-signed JSON in the AWS record format, retries with backoff.
- Import and export transfers to and from any S3 endpoint, resumable by
  re-running, with dry run, prefix mapping and progress.
- Cellar: approved-join groups of servers with full copies everywhere,
  synchronous write acknowledgement, backfill, catch-up readiness and key
  rotation, formed in three commands.
- Replication per bucket or server-wide, to one or many servers or any S3
  endpoint, one-way or as an active-active mesh (changes are never forwarded
  twice), replaying versions, delete markers and bucket configuration in
  order, with lag and last error per target, pause/resume for failover,
  destination buckets created on demand, and optional sync of users, keys
  and settings. Servers find each other on the LAN.
- Prometheus metrics, structured logs, health endpoints, an optional JSON
  access log, and hourly usage counters per bucket, user, access key and
  operation class (`hogshead usage`, the Usage page).
- Background workers: lifecycle sweep, orphan chunk GC, multipart cleanup,
  replication.

## Architecture

```
                     ┌──────────────────────────────────────────┐
  S3 clients ──────▶ │  HTTP server (net/http)                  │
  Web UI ──────────▶ │  ├─ api/     S3 routing, XML, errors     │
                     │  ├─ auth/    SigV4, keys, bucket policy  │
                     │  ├─ ui/      embedded SPA                │
                     │  └─ admin/   JSON admin API              │
                     └───────────────┬──────────────────────────┘
                                     │
                     ┌───────────────▼──────────────────────────┐
                     │  meta/   metadata store                  │
                     │  every mutation is a Command, applied    │
                     │  through one Apply() to SQLite (WAL)     │
                     │  and recorded in the replication log     │
                     └───────────────┬──────────────────────────┘
                                     │
                     ┌───────────────▼──────────────────────────┐
                     │  store/  chunk store                     │
                     │  content-addressed (SHA-256), chunked,   │
                     │  optional zstd, optional AES-GCM         │
                     └──────────────────────────────────────────┘
                     ┌──────────────────────────────────────────┐
                     │  replication/  standby targets           │
                     │  replays the log to other servers via S3 │
                     └──────────────────────────────────────────┘
```

**Language and runtime.** Go, standard library HTTP server, no CGO so the
release is one static binary per platform. The web UI is SvelteKit with
`adapter-static`, built to plain files and embedded with `embed`.

**Admin surface.** The admin JSON API under `/_admin` is the only management
interface. `hogshead` CLI subcommands call it over HTTP (local socket or
remote URL), so the CLI works against any node from anywhere. The UI uses the
same endpoints. Each admin feature lands as API, then CLI, then UI.

**Metadata.** SQLite with WAL. Buckets, objects, versions, multipart state,
users, keys, policies, lifecycle, CORS, certificates. Every write is expressed
as a serialisable `Command` and goes through a single `Apply(cmd)` function,
which also appends object changes to a replication log. Keeping the write
path in one place is what makes replication a consumer of a log instead of a
set of hooks.

**Object data.** Objects are split into chunks (default 4 MiB), each chunk
stored under the data dir by SHA-256 of its plaintext. An object version is a
list of chunk hashes plus size and ETag. Content addressing gives free
deduplication, cheap CopyObject, and idempotent replication by
hash comparison. Multipart parts are just chunk lists that get concatenated on
complete. Orphan chunks are swept by a GC worker.

**Workers.** Lifecycle, GC, multipart cleanup and replication are background
goroutines inside the server process.

### Repository layout (planned)

```
cmd/hogshead/        main, subcommands (serve, quickstart, user, key, bucket)
internal/api/         S3 handlers, XML marshalling, S3 error codes
internal/auth/        SigV4 verification, key lookup, policy evaluation
internal/meta/        Command types, Apply, SQLite schema and migrations
internal/store/       chunk store, filesystem backend, compression, encryption
internal/replication/ standby targets, change-log replay, signing S3 client
internal/worker/      lifecycle, gc, multipart cleanup
internal/admin/       admin JSON API used by the UI and CLI
internal/config/      config file, env overrides, validation
internal/ui/          embed of the built web UI
ui/                   web UI source
test/conformance/     s3-tests runner, client matrix scripts
docs/                 design notes that are too long for this README
```

## Replication and failover

Each server is independent. To keep a copy of a bucket elsewhere, add one or
more standby targets; a target is an S3 endpoint, a bucket and credentials,
and can be another hogshead server or any S3-compatible service.

```
# first standby
hogshead bucket replicate photos --to https://standby1.lan:9000 \
  --bucket photos --dest-key AK --dest-secret SK --path-style

# a second one, named, only a prefix
hogshead bucket replicate photos --name offsite --to https://s3.example.com \
  --bucket photos-mirror --dest-key AK --dest-secret SK --prefix raw/

hogshead bucket replicate                      # lag and last error per target
hogshead bucket replicate photos --name offsite --disable   # pause one target
hogshead bucket replicate photos --remove                   # drop them all
```

To mirror the whole server instead, add a server-wide target. Every bucket,
including ones created later, replicates to a bucket of the same name on the
standby, which is created there on first use (versioned when the source is):

```
hogshead replicate --to https://standby.lan:9000 --dest-key AK --dest-secret SK --path-style
hogshead replicate                              # targets and per-bucket lag
hogshead replicate --name standby.lan:9000 --disable
```

A bucket with its own target of the same name overrides the server-wide one.
Bucket deletion, policies, lifecycle and CORS are not replicated; only
object changes are.

### Migrating data in or out

Transfers are one-shot copies between a bucket here and a bucket on any
S3-compatible service, in either direction. Use an export to migrate away or
to seed a standby before enabling replication, and an import to migrate in.

```
hogshead transfer export photos --to https://s3.amazonaws.com --virtual-host \
  --bucket my-archive --dest-key AK --dest-secret SK --region eu-west-1 --wait
hogshead transfer import photos --from https://old-server:9000 --bucket photos \
  --dest-key AK --dest-secret SK --prefix 2025/ --remote-prefix archive/2025/ --dry-run
hogshead transfer list                       # progress of every job
hogshead transfer cancel t-1a2b3c4d5e6f      # stop a running job, or drop a finished record
```

Objects whose size and ETag already match at the destination are skipped, so
a job can be re-run to catch up; `--overwrite always|never` changes that.
Metadata, content type and tags travel with each object; only current
versions are copied, bucket settings are not. Jobs run inside the server
with configurable concurrency and leave a record with counts and the first
failures. The same jobs are available in the admin API under `/transfers`
and on the Transfers page of the web UI.

How it works: every object put, delete marker and delete is appended to a
replication log in the metadata store. The replication worker replays the
log for each enabled target in order with ordinary SigV4 `PutObject` and
`DeleteObject` calls, advances a cursor per target, and trims the log below
the lowest cursor. A failed push stops that target until the next pass (every
10 seconds) and shows as `last_error`; `lag` is the number of entries not yet
pushed. A paused target keeps its pending entries in the log until it is
resumed or removed, so pause for a failover, not for months. Use a versioned
bucket on the standby so delete markers map one to one; purging an old version on the source is not replicated because S3 has no
portable way to address it.

Failover is manual and symmetric: pause the target, point clients at the
standby, and when the original server is back add the reverse target there.
Nothing is enforced on the standby; it is a normal bucket on a normal server.

Bucket configuration travels too: a destination bucket is created with the
source's Object Lock and versioning flags, and later changes to versioning,
policy, lifecycle, CORS, tagging, lock defaults and notifications follow.
So does per-object Object Lock: a replicated version carries its retention
and legal hold, and later retention, legal-hold and tag changes are replayed
to the same version on the other side.

### Cellar

A cellar is the easy way to run several servers as one: every member holds a
full copy of every bucket, user and setting, any member serves reads and
writes, and there is no leader, quorum or consensus log.

```
hogshead cellar init --name office            # on the first server
hogshead cellar join 192.0.2.10              # on each other server
hogshead cellar approve <server-id>           # on any member, or click Approve in the UI
hogshead cellar status
```

Servers see cellars on the LAN, so the UI offers "Join office" directly.
Nothing joins without approval, and a server must be empty to join unless
you ask for a merge, in which case the cellar's configuration and users win
any clash and the newest version of a key is current everywhere. A new member receives a backfill of all
data, users and keys; a member that was down catches up when it returns and
reports not-ready on `/readyz` until it has. Writes are acknowledged only
after at least one other member holds them (`write_ack = one`; `all` and
`local` are available), so no single failure loses an acknowledged write.
Replicated changes carry their original version id and time, so delivering
a change twice is a no-op and a delayed change never overtakes a newer one.
Removing a member rotates the cellar key. See ADR 0007.

### Two-way and multi-way replication

Every change a server receives from a peer is tagged with the peer's id and
is never forwarded again, so servers can replicate to each other freely. Two
servers with server-wide targets pointing at each other form an
active-active pair; three or more that all target each other form a mesh in
which every change travels exactly one hop. Writes to different keys merge
cleanly. Concurrent writes to the same key on two servers end up as two
versions on each side with a different "latest" per side, so use a mesh for
disjoint writers or failover rather than for simultaneous edits of one key.

### Users and settings

A server-wide target can also mirror users (with their passwords), access
keys and server settings, so a standby is ready to serve the same clients:

```
hogshead replicate --to http://hogshead2:9000 --dest-key AK --dest-secret SK \
  --path-style --sync-config
```

The key must belong to an administrator on the target. Replication state,
the web UI session secret, the server identity and per-bucket settings are
never mirrored.

### Finding servers on the LAN

Servers announce themselves by UDP multicast on 239.255.77.77:9470 (off with
`discovery.enabled = false`). `hogshead peers` lists what was heard, the
Replication target dialog offers the same list, and `name = "hogshead1"` in
the config gives each server a human-friendly label shown in status, the UI
and the beacon.

### Consistency

A server acknowledges a write once its data and metadata are durable locally.
Replication is asynchronous; the standby trails by the reported lag, usually
seconds. Lifecycle, GC and replication run as background workers in the same
process.

## Configuration

Node-local file (`/etc/hogshead/config.toml`), overridable by `HOGSHEAD_*`
environment variables:

```toml
name       = "hogshead1"          # label shown in status, the UI and on the LAN
access_log = "/var/log/hogshead/access.log"   # optional, one JSON line per request
data_dir   = "/var/lib/hogshead"
listen     = ["0.0.0.0:9000"]
hostname   = "s3.example.com"          # for virtual-host style

[tls]
mode = "acme"                          # "acme" | "file" | "off"
acme_email = "ops@example.com"

```

Everything else (users, keys, passwords, buckets, policies, lifecycle, CORS,
webhooks, replication targets, worker tuning) is server state managed through
the CLI, the admin API or the UI.

### Web UI login

The UI accepts either an access key, which it uses to sign requests in the
browser, or a user name and password. Passwords are bcrypt hashes stored with
the user (`hogshead user passwd <name>`, the Users page, or
`PUT /_admin/v1/users/{name}/password`); quickstart sets one for the admin.
A password login sets an HttpOnly, SameSite=Strict cookie signed with a
per-server secret (`ui.session_secret`, created on first start) that the S3
and admin APIs accept in place of SigV4 for same-origin requests carrying the
`X-Hogshead-Session` header. Failed logins are rate limited per address. The
cookie is only marked Secure when TLS is on, so put TLS in front of any UI
reachable beyond a trusted network.

### Live reconfiguration

Server state (users, buckets, settings) changes take effect immediately.

The node-local file is polled and also re-read on `SIGHUP` or
`hogshead config reload`. Changes to log level, certificate files, ACME
domains, rate limits and worker tuning apply in place; certificates are
hot-swapped without dropping connections. A change to `listen`, `data_dir`,
`tls.mode`, `storage.key_file` or `admin.socket` is logged as "requires restart" and ignored until the next
start. The admin API and `hogshead status` report which local settings are
pending a restart.

## Compatibility targets

CI runs each of these against a fresh instance on every PR:

| Client | What we test |
|--------|--------------|
| `aws` CLI | bucket and object CRUD, multipart via `s3 cp`, presign |
| `rclone` | sync, check, `--s3-upload-cutoff` multipart, versioning |
| `restic` | init, backup, check, prune |
| `s3cmd` | put, get, ls, setpolicy, setcors |
| Velero | backup and restore to a bucket |
| `s3-tests` conformance suite | the subset tagged for the features above |

## Libraries

| Need | Library | License |
|------|---------|---------|
| SQLite | [modernc.org/sqlite](https://gitlab.com/cznic/sqlite), pure Go | BSD-3 |
| ACME certificates, including DNS-01 | [certmagic](https://github.com/caddyserver/certmagic) + [libdns](https://github.com/libdns/libdns) | Apache-2.0 / MIT |
| zstd compression | [klauspost/compress](https://github.com/klauspost/compress) | BSD-3 |
| Conformance tests | the `s3-tests` suite with a curated allowlist (`test/conformance`) | MIT |

Request routing, SigV4 verification, the XML model and checksum combination
are implemented in this repository on the standard library. Clustering designs
were explored and then removed; see ADR 0004 and 0005.

## Decisions

Settled:

| Topic | Decision |
|-------|----------|
| License | MIT |
| Several servers | A cellar (full copy on every member, no consensus) or bucket replication to a standby; no shared-metadata cluster (ADR 0005, 0007) |
| Language | Go, no CGO |
| Metadata | SQLite (`modernc.org/sqlite`), WAL |
| Object layout | Content-addressed chunks, SHA-256, 4 MiB default |
| Metadata writes | One `Apply` into SQLite, no consensus layer (ADR 0005) |
| Replication | Per-bucket standby targets replayed from a change log over the S3 API |
| Management | Admin JSON API first; CLI and web UI are clients of it; full parity |
| Web UI | SvelteKit, `adapter-static`, embedded |
| Site | SvelteKit static site in the separate `site` repo |
| Postgres metadata backend | No |

Also settled:

| Topic | Decision |
|-------|----------|

Deferred to 1.x: Object Lock (WORM), event notifications via webhook.

## Milestones

See [TASKS.md](TASKS.md) for the detailed breakdown.

1. Skeleton, core API, SigV4, filesystem chunk store, SQLite metadata.
2. Multipart, versioning, presigned URLs, range and conditional requests.
3. Users and roles, bucket policy, CORS, lifecycle, ACME TLS, web UI.
4. Conformance suite, encryption at rest, audit log, metrics, 1.0 release.
5. Bucket replication to standby servers.

## License

MIT. See [LICENSE](LICENSE).
