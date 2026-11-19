# 0004: Single-site clusters with full replication; replication across sites

Status: superseded by 0005
Date: 2026-10-10

## Context
ADR 0002 chose zone-aware placement with a per-chunk replication factor and
per-bucket consistency policies so one cluster could span two sites. The
review of that implementation found most of the data-loss edge cases in
placement changes, draining and surplus deletion, and the operator had to
understand zones, voters and two consistency settings. The user asked for a
simpler binary.

## Decision
A cluster is a set of 2 to 7 nodes on one network. Every node is a Raft voter
and stores every chunk. Writes go to all nodes and are acknowledged by a
majority; reads are always local, with a fallback to any peer while a node is
catching up. Repair is each node pulling the chunks it lacks. Removing a node
is immediate. Spanning sites is done by active-passive bucket replication
over the S3 API to another cluster (or any S3 target), not by stretching a
cluster. Status warns when a peer answers slowly, since a stretched cluster
still works but pays WAN latency on every write.

## Consequences
Disk use is one full copy per node; that is the price of not having placement.
Zones, `replication_factor`, `write_consistency` and `partition_reads` are
gone. The cluster package shrank by about half and the repair worker can no
longer delete the last copy of anything. Cross-site durability depends on the
replication worker's lag, which status reports per rule.
