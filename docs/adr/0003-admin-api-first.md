# 0003: Admin API first, CLI and UI are clients

Status: accepted
Date: 2026-10-08

## Context
Everything must be doable from the API and from the command line, and config
changes must not need a restart.

## Decision
All management goes through the admin JSON API. The CLI calls it over HTTP
(unix socket locally, URL remotely). The web UI calls the same endpoints. CI
checks that every admin endpoint has a CLI subcommand. Cluster state is live
by construction; the local config file is watched and hot-reloaded, with only
listen addresses, data dir and cluster advertise requiring a restart.

## Consequences
No management logic lives in the CLI. Remote administration works out of the
box. Local-file settings must be classified as live or restart-only.
