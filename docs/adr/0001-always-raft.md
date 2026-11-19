# 0001: Metadata always goes through Raft

Status: superseded by 0005
Date: 2026-10-08

## Context
hogshead must run as a single node by default and as a small cluster with
one-command join. Two metadata code paths (direct SQLite and Raft) would
double the surface for bugs and make cluster mode the less tested path.

## Decision
Every metadata mutation is a `meta.Command` applied by one function,
`meta.DB.Apply`. That function is only ever called by the Raft FSM. A single
node runs a one-voter Raft group. The applied log index is stored inside
SQLite in the same transaction as the mutation so replay after restart is
idempotent. Snapshots are `VACUUM INTO` copies of the database.

## Consequences
Standalone nodes carry a Raft log and periodic snapshots on disk. Cluster mode
adds membership and data replication but no new metadata path. All config is
metadata, so it syncs for free.
