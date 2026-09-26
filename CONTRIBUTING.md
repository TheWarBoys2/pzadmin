# Contributing

Bug reports, ideas and pull requests are all welcome.

## Reporting a bug

[Open an issue](https://github.com/TheWarBoys2/pzadmin/issues/new/choose) and
fill in the form. The most useful things are the PZAdmin version, what you
did, what you expected and what happened, and anything relevant from
`docker logs pzadmin`. Please remove passwords, tokens and webhook links
before pasting logs.

Security problems go through [SECURITY.md](SECURITY.md) instead.

## Making a change

PZAdmin is one Go binary using only the standard library, with a plain
JavaScript interface in `web/` that needs no build step. You need Go 1.27, and
Node.js to run the frontend tests.

Before opening a pull request, run what CI runs:

```sh
gofmt -l .                                              # should print nothing
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
go test -race ./...                                     # includes the frontend tests when node is installed
```

A few things that keep the project easy to run and trust:

- **No new dependencies** unless there is no reasonable way around it.
- **No Docker socket.** Container control goes through Arcane.
- **Say what is true.** Error messages, docs and interface text should tell
  people exactly what happened and what to do next, in plain words.
- **Add a test** for a bug fix or new behaviour.
- **Update the docs** (README, `docs/api.md`, `CHANGELOG.md`) when behaviour
  changes.

To try your change in Docker:

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

By contributing you agree your work is released under the [MIT licence](LICENSE).
