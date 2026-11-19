# 0007: Cellar: automated multi-master groups without consensus

Status: accepted
Date: 2026-10-19

## Context
Mesh replication (ADR 0006) made active-active groups possible but left
every target, credential and flag to be configured by hand on every server,
and it stayed asynchronous: a server that died right after acknowledging a
write took that write with it. Users wanted something as simple to form as a
container swarm, with approval before a server is admitted, full copies on
every member, and users and keys that follow.

## Decision
A cellar is a replicated setting (the member list) plus a service account.
`cellar init` on one server creates the cellar id and name, an admin user
`cellar` with one access key, and a member list of one. A server joins by
asking any member over an unauthenticated endpoint; the request is stored as
a replicated setting so an administrator can approve it on any member; the
joiner polls with a nonce and, once approved, receives the cellar id, member
list and service key. Because users, keys and settings replicate, every
member derives its own replication targets from the member list with a small
reconcile loop: one server-wide target per other member, with config sync.
Removing a member rotates the service key so the removed server loses access.
There is no leader, no quorum and no consensus log.

Three replication changes make this safe:

- Replicated puts and markers carry their source version id and time. A
  version already held is a no-op, so any change can be delivered twice, and
  "latest" follows the original write time, so a change delayed by an outage
  never overtakes a newer one.
- Changes are pushed when committed, not on a timer, and the cellar's
  `write_ack` level makes the S3 API push a write to every member before
  committing it, answering only once enough members confirmed (`one` by
  default; `all` or `local` are available).
- A new member receives a backfill of current state rather than a replay,
  and a returning member drains its backlog; until the backlog is zero the
  readiness endpoint says so.

## Consequences
Every member stores everything, so capacity is that of the smallest member
and it does not grow with members. Concurrent writes to the same key on two
members keep both versions and order them by time. A write acknowledged at
level `one` survives any single member failure; with `local` a member that
dies within seconds of acknowledging can lose that write. Approving a server
hands it administrator credentials for the whole cellar. Cellar settings
live under the Cellar menu and `cellar.*` keys; replication to external S3
endpoints stays a separate feature.
