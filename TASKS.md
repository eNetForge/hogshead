# TASKS

Work breakdown for hogshead. Phases are ordered; items within a phase are
roughly ordered. Each item should land with tests. Tick items as they merge.

Legend: `[ ]` todo, `[~]` in progress, `[x]` done, `[-]` dropped.

## Phase 0: Project skeleton

- [x] License: MIT, `LICENSE` added
- [x] `go mod init`, Go version pin, `.gitignore`, `Makefile` (build, test,
      lint, ui, release)
- [x] `golangci-lint` config, `gofumpt`
- [x] GitHub Actions: lint, unit tests with race detector, coverage gate 80%
- [x] `goreleaser` config for linux/amd64, linux/arm64, darwin, windows,
      static binaries, no CGO
- [x] `cmd/hogshead` with `serve`, `version`, `quickstart` stubs
- [x] CLI talks to the admin API over HTTP (unix socket by default, `--server`
      for remote); no CLI command touches SQLite directly except `serve`
- [x] `internal/cluster` skeleton: hashicorp/raft with one voter, FSM
      wrapping `meta.Apply`, boltdb log store, `VACUUM INTO` snapshots.
      Always on from day one so Phase 5 adds membership, not Raft
- [x] `internal/config`: TOML file, `HOGSHEAD_*` env override, validation,
      `--config` flag, sensible defaults
- [x] Structured logging (`log/slog`), request IDs
- [x] Live reload of the local config: fsnotify watch, `SIGHUP`,
      `hogshead reload`; settings classified as live or restart-only;
      restart-only changes logged and reported by the admin API
- [x] `docs/` with ADR template, record decisions from README

## Phase 1: Core API, SigV4, storage, metadata

### Metadata
- [x] `internal/meta`: SQLite via pure-Go driver, WAL, migrations
- [x] Schema: buckets, objects, versions, chunks, multipart, users, keys,
      policies, lifecycle, cors, settings
- [x] `Command` types for every mutation, one `Apply(cmd)` entry point,
      deterministic (no wall clock or randomness inside Apply; pass them in)
- [x] Read-side queries: get object, list objects with prefix/delimiter/
      pagination, list buckets
- [x] Unit tests for Apply determinism (replay a log twice, compare DB)

### Chunk store
- [x] `internal/store`: chunk interface (`Put(reader) hashes`, `Get(hash)`,
      `Has`, `Delete`)
- [x] Filesystem backend: `data/chunks/ab/cd/<sha256>`, write to temp and
      rename, fsync, verify on read
- [x] Chunker: fixed 4 MiB, configurable
- [x] Optional zstd per chunk, header byte records compression
- [x] Property tests: round-trip, truncated reads, corrupted chunk detection

### Auth
- [x] `internal/auth`: SigV4 header auth, canonical request, signed headers,
      `UNSIGNED-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` (aws-chunked)
- [x] Clock skew window, constant-time compare
- [x] Key lookup from metadata, inactive key handling
- [x] Failed-auth rate limit per source IP
- [x] Test vectors from AWS docs plus captured requests from aws cli, rclone,
      restic, s3cmd

### S3 API
- [x] `internal/api`: router for path-style and virtual-host-style, S3 XML
      encoders, S3 error codes and bodies, request ID headers
- [x] Bucket: CreateBucket, DeleteBucket, HeadBucket, ListBuckets,
      GetBucketLocation
- [x] Object: PutObject, GetObject, HeadObject, DeleteObject, DeleteObjects
- [x] ListObjectsV2 and ListObjects (V1) with prefix, delimiter,
      continuation token, max-keys, encoding-type
- [x] CopyObject with metadata directive
- [x] ETag: MD5 for single PUT, AWS-style `md5(md5s)-N` for multipart
- [x] Smoke test: `aws s3 mb/cp/ls/rm` against a test server in CI

### Quickstart
- [x] `hogshead quickstart`: data dir, admin user and key, config file,
      optional systemd unit, prints next steps

## Phase 2: Multipart, versioning, presigned, ranges

- [x] Multipart: CreateMultipartUpload, UploadPart, UploadPartCopy,
      CompleteMultipartUpload, AbortMultipartUpload, ListParts,
      ListMultipartUploads
- [x] Part size validation, part ordering, ETag per part
- [x] Range GET including suffix ranges, 416 handling, multipart objects
- [x] Conditional headers: If-Match, If-None-Match, If-Modified-Since,
      If-Unmodified-Since on GET, HEAD, PUT, Copy
- [x] Versioning: PutBucketVersioning, GetBucketVersioning, version IDs,
      delete markers, `versionId` on GET/HEAD/DELETE, ListObjectVersions
- [x] Presigned URLs: SigV4 query auth, expiry, GET and PUT
- [x] Checksums: `x-amz-checksum-sha256`, `x-amz-checksum-crc32c`, trailing
      checksums, `x-amz-checksum-mode` on GET
- [x] Multipart cleanup worker (abort stale uploads)
- [x] Orphan chunk GC worker with grace period
- [x] CI: rclone sync and check, restic backup and check

## Phase 3: Users, policy, CORS, lifecycle, TLS, UI

- [x] Users and keys CLI: `user add/rm/list`, `key create/revoke/list`
- [x] Roles: admin, read-write, read-only; optional bucket scope
- [x] Bucket policy subset: parse, validate, evaluate; public read; allow and
      deny by principal, bucket and prefix. Reject unsupported statements
      with a clear error
- [x] CORS: Put/Get/DeleteBucketCors, preflight handling
- [x] Lifecycle: Put/Get/DeleteBucketLifecycle; expiration by age,
      noncurrent version expiry, abort incomplete multipart; leader worker
- [x] TLS: supplied cert, ACME HTTP-01, ACME DNS-01 (pluggable providers),
      cert storage in metadata so cluster nodes share it, hot swap of certs
      and TLS mode without restart
- [x] Admin JSON API under `/_admin` with the same auth; OpenAPI document
      generated from the handlers
- [x] CLI parity check in CI: every admin endpoint has a CLI subcommand
- [x] Web UI (SvelteKit, adapter-static): bucket browser, upload
      (multipart), download, presigned link sharing, bucket settings, users
      and keys, server status
- [x] UI embedded in binary, served at `/_ui`; `make ui` builds it, CI checks
      the embedded build is current
- [x] Audit log of admin and auth events (file and metadata)

## Phase 4: Conformance, hardening, 1.0

- [x] `s3-tests` conformance runner in CI with an allowlist of tests we claim
- [x] Client matrix in CI: aws cli, rclone, restic, s3cmd, boto3 (Velero needs Kubernetes; manual procedure documented)
- [x] Encryption at rest: AES-256-GCM per chunk, local master key, key file
      permission checks
- [x] Prometheus metrics, `/healthz`, `/readyz`
- [x] Rate limiting and request size limits
- [x] Fuzz tests for XML and SigV4 parsing
- [x] Error messages reviewed for leaks
- [x] Docs: install, config reference, client guides (one page per client),
      backup and restore of the data dir
- [ ] 1.0 release

## Phase 5: Cluster mode

### Foundations
- [x] ADRs recording the settled decisions in README
- [x] Raft transport on cluster port, mTLS, cluster CA generation, node certs
- [x] Restore path from snapshot swaps the DB file atomically
- [x] Followers forward metadata writes to the leader; no client redirects
- [x] Deterministic replay test: same log on three fresh nodes gives
      identical DBs

### Membership
- [x] Join tokens: `<base32(sha256(CA))>.<secret>`; generate, print,
      rotate secret only, optional expiry
- [x] `cluster join <addr> --token --zone`: verify, issue node cert, add as
      voter or non-voter, stream snapshot
- [x] `cluster node set <node> --zone`: relabel, triggers placement recompute
- [x] `cluster status`: nodes, roles, leader, lag, disk, repair backlog
- [x] Unreachable nodes are flagged in status with how long they have been gone; never auto-removed
- [x] `cluster remove <node>`: mark draining, wait for repair, remove from
      Raft config
- [x] `cluster leave`: same from the node itself
- [x] Voter promotion rules: keep 1, 3 or 5 voters automatically

### Data replication
- [x] Placement: zone-aware rendezvous hashing (top node per zone first,
      then global ranking), RF stored in cluster state; property test that
      every zone gets a replica when zones <= RF and that a node change moves
      only the affected chunks
- [x] Write path: coordinator streams to RF replicas in parallel, ack per
      bucket `write_consistency` (`majority`, `all`, `each_zone`), version
      commit through Raft after ack
- [x] Bucket settings `write_consistency` and `partition_reads` in metadata,
      admin API, CLI (`bucket set`), UI
- [x] Read path: local replica first, proxy to a holder otherwise, retry on
      failure
- [x] Repair worker on leader: compare placement vs holders, copy missing
      chunks, delete surplus after placement change, rate limited
- [x] Chunk inventory per node for repair (local index, rebuildable from disk)

### Behaviour under failure
- [x] One node down of three: reads and writes pass (integration test with
      three in-process nodes)
- [x] Minority partition: writes 503, reads per `partition_reads`
- [x] Two-zone test (3 + 2): kill the 3-node zone, verify every object is
      still readable from the 2-node zone with `partition_reads = stale`
- [x] Dead node replacement runbook and test
- [x] Rolling upgrade test

### Operations
- [x] UI: cluster page with node health
- [x] Docs: cluster setup, DNS round robin and VIP guidance, failure runbooks
- [ ] 1.1 release with cluster mode

## Later / undecided

- [x] Object Lock (WORM): bucket flag, retention, legal hold, default retention rules, delete enforcement, governance bypass permission
- [x] Event notifications via webhook (QueueConfiguration with arn:hogshead:webhook:::<name>, URLs in cluster settings, HMAC-signed JSON, retries)
- [ ] Per-bucket replication factor (needs max-RF resolution for shared chunks)
- [ ] S3 Select: no
- [-] Postgres metadata backend: no

## Phase 6: Simplification (October 2026)

- [x] Replace zone-aware placement with full replication (every node holds every chunk)
- [x] All nodes are voters; remove voter management and draining
- [x] Remove `write_consistency`, `partition_reads`, `replication_factor`, zones
- [x] Pull-based repair on every node; immediate node removal with revocation
- [x] Slow-peer warning in `cluster status`
- [x] Active-passive bucket replication to another cluster or S3 target
- [x] Trim: Route 53 DNS provider, numeric/null policy operators, unused bucket stubs (requestPayment, accelerate, website)

## Phase 7: No clustering (October 2026)

- [x] Remove Raft, the cluster package, cluster port and PKI, cluster CLI/API/UI
- [x] Metadata commands apply directly to SQLite
- [x] Replication: one or many standby targets per bucket, each with its own cursor, pause/resume, remove one or all
- [x] ADR 0005 supersedes 0001 and 0004
- [ ] Docs site updated for standalone servers and multi-target replication

## Phase 8: hogshead (October 2026)

- [x] Rename the project to hogshead (module, binary, env prefix, ARN, headers, metrics, docs)
- [x] Server-wide replication target: every bucket mirrors to a same-named bucket on the standby, auto-created
- [x] Replication in the web UI: per-bucket card, Replication page with server-wide targets, dashboard summary
- [x] Bucket browser: drag-and-drop upload (files and folders), inline preview, text editor, New file
- [x] Web UI password login: bcrypt passwords per user, HMAC session cookie accepted by the S3 and admin APIs with same-origin checks, `user passwd`, quickstart admin password, pure-JS SHA-256 so access-key login also works on plain HTTP
- [x] Transfers: import/export jobs to and from other S3 services (admin API, CLI, web UI), shared s3client package
- [x] Server name and id; LAN discovery beacon with peers in API, CLI and the target dialog
- [x] Mesh replication: replicated changes carry their origin and are never forwarded (ADR 0006)
- [x] Bucket configuration replication and lock-aware destination buckets
- [x] Server-wide targets can sync users, keys, passwords and settings (`--sync-config`)

## Phase 9: Cellar (October 2026)

- [x] Idempotent, timestamp-ordered replication (shared version ids, source time, versioned purges)
- [x] Push-on-write and `write_ack` (local | one | all) with pre-commit push to cellar members
- [x] Cellar membership: init, join with approval, leave, remove with key rotation, backfill, readiness gate, bucket deletion mirrored (ADR 0007)
- [x] Cellar menu in the UI (create, join from discovered cellars or by address, approve, members, settings); cellar targets hidden from Replication
- [x] Per-object Object Lock replication (retention, legal hold, tags follow the version)
- [x] Access log file and hourly usage counters (API, CLI, Usage page, per-bucket card)
- [x] Object tagging events (`s3:ObjectTagging:Put|Delete`) are emitted
- [x] A destination that refuses bucket configuration (403) no longer stalls a replication target; warning on the target instead
- [x] `user logout-all` / `POST /sessions/rotate` / Users page: replace the web UI session secret
