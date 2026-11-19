# 0006: Mesh replication by origin tagging, configuration sync, LAN discovery

Status: accepted (extended by 0007)
Date: 2026-10-19

## Context
Replication (ADR 0005) was strictly one-way: a server replayed its own
change log to standby targets. Users asked for two- and three-way
replication, for users and settings to follow buckets to a standby, and for
servers to find each other on a LAN. Any multi-master arrangement needs a
rule that stops a change from bouncing between servers forever.

## Decision
Every server has a random persistent id (`server.id`). The replication
client tags each push with `X-Hogshead-Origin: <id>`; a hogshead server
receiving it records the origin on the resulting log entries. The worker
never forwards an entry that carries an origin. A set of servers that all
target each other therefore forms a loop-free mesh: each change travels one
hop, from the server where it was made to every peer. Star or chain
topologies do not forward, by design.

Bucket configuration (creation flags, versioning, policy, lifecycle, CORS,
tagging, Object Lock defaults, notifications) is logged as bucket entries and
replayed to every target of that bucket; a destination bucket is created with
the source's lock and versioning flags and its current configuration.

Server-wide targets may opt into configuration sync. Users (with password
hashes), access keys (with secrets) and replicable settings are logged under
an empty pseudo-bucket and replayed through the target's admin API, which
gained wholesale import routes. Replication state, the web UI secret, the
server id and per-bucket settings never sync.

Servers announce themselves by UDP multicast (239.255.77.77:9470) and list
peers heard recently; the list only pre-fills forms.

## Consequences
Concurrent writes to the same key on two servers produce two versions on
each side with a different "latest" per side; the model is last writer wins
per server, suited to disjoint writers or failover, not to concurrent edits
of one key. Deleting a bucket is still not replicated. Config sync requires
the target credentials to be an administrator there and makes the mesh a
single administrative domain.
