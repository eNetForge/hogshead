# 0002: N-way replication with zone-aware rendezvous placement

Status: superseded by 0004
Date: 2026-10-08

## Context
Users want to run three nodes at home and two at an office and be certain the
office always holds a copy. Erasure coding is out of scope.

## Decision
Each chunk is stored on `RF` nodes. Nodes carry a `zone` label. Placement
ranks nodes by rendezvous hash of the chunk hash, takes the best node from
each zone first, then fills remaining slots from the global ranking. Per-bucket
`write_consistency` (`majority`, `all`, `each_zone`) decides when a PUT is
acknowledged. `RF` is cluster-wide because chunks are deduplicated across
buckets.

## Consequences
Every zone holds at least one copy when zones <= RF. Raft quorum is not
zone-aware, so in a two-zone cluster the minority zone is read-only during a
partition.
