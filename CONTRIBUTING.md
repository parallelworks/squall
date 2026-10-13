# Contributing to squall

- Every change goes through a pull request against `canary`.
- PR titles follow [Conventional Commits](https://www.conventionalcommits.org)
  (`feat(compute): ...`, `fix(fault): ...`, `docs: ...`). PRs are squash-merged, so
  the title becomes the commit and the changelog entry.
- Packages import only the Go standard library. Client libraries a stand-in is
  tested against are test dependencies, but go.mod still lists them, so their
  versions become the minimum for every module that requires squall.
- A new method or service is tested through the official Go client library for
  that API, so the stand-in is held to what the client actually sends and parses.
  Its package documentation says where it differs from the real API.
- A stand-in answers what it does not implement with 404, and rejects input it
  does not understand (an unsupported filter, say) with 400 rather than guessing.
- Run `make check` (lint, vulnerability check and tests with the race detector)
  before opening a PR. Changes to request handling include benchmark results
  (`make bench`) against `canary`.
