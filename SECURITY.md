# Security

PZAdmin controls game servers and can run admin commands on them, so security
problems matter. Thank you for looking.

## Reporting a problem

Please **don't open a public issue** for a security problem. Report it
privately instead:
[Report a vulnerability](https://github.com/TheWarBoys2/pzadmin/security/advisories/new).

Say what you found, how to reproduce it, and which version you're running (it is
shown under the PZAdmin logo, and in `docker logs pzadmin`). This is a hobby project, so replies may take a few
days, but every report is read and answered.

## Supported versions

Only the latest release gets fixes. Update with `docker compose pull` and
`docker compose up -d`.

## What PZAdmin is designed to protect against

- Someone on your network who does not have the admin password, including
  before setup is finished (setup needs a code from the container log).
- A web page in your browser trying to act on PZAdmin (CSRF protection,
  same-site cookies).
- A leaked export file or API key file (exports hold no secrets; API keys are
  stored hashed).
- An API key doing more than it was given (scopes and per-server limits).

## What it is not designed for

- Being exposed directly to the internet. It serves plain HTTP. Use a VPN, or
  at least a reverse proxy with HTTPS (see the README).
- Several admin accounts with different permissions. There is one admin.
- Protecting against someone who can already run commands on the Docker host.
