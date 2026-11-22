# Security policy

## Reporting a vulnerability

Please report security problems privately, not in a public issue.

Use GitHub's private reporting: open the **Security** tab of this repository
and choose **Report a vulnerability**. That creates a private advisory only
the maintainers can see.

Include what you can of:

- the hogshead version (`hogshead version`) and how it is deployed,
- what an attacker can do and what access they need first,
- steps or a script that reproduce it.

You will get an acknowledgement within a few days. Once a fix is released the
advisory is published, with credit to the reporter unless you ask otherwise.

## Supported versions

Fixes go into the latest release. Upgrade to it before reporting, if you can.

## Scope

In scope: the server, its S3 and admin APIs, the web UI, the CLI and the
published container image. Problems that need administrator credentials or
write access to the data directory to begin with are generally not treated
as vulnerabilities.
