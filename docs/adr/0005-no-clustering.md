# 0005: No clustering; standalone servers with per-bucket standby replication

Status: accepted (supersedes 0001 and 0004)
Date: 2026-10-12

## Context
Even the simplified cluster (ADR 0004) carried Raft, mutual TLS, a join
protocol, membership, repair and the operational questions that come with
them. The user decided the product should be a set of independent servers,
with durability across machines coming from replicating buckets to one or
more standby servers.

## Decision
A hogshead server is standalone. Metadata commands apply directly to SQLite
under a mutex; there is no Raft log. Each bucket can have any number of
replication targets, each an S3 endpoint plus bucket and credentials. A
replication worker replays the bucket's change log (puts, delete markers and
deletes) to every enabled target in order, keeping a cursor per target and
reporting lag and last error. Targets can be paused for failover and resumed.
The passive side is an ordinary bucket on an ordinary server, so failover is
pointing clients at it and reversing the rule later.

## Consequences
The binary loses the cluster package, Raft and bbolt, the cluster port and
its PKI, and the cluster CLI and UI. A server has no high availability of its
own; the answer to "a server died" is "point clients at a standby". Writes no
longer wait on a consensus round. The metadata command log and its
determinism rules remain because they are cheap and keep the write path in
one place.
