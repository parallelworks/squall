# Contributing to squall

- Every change goes through a pull request against `canary`.
- PR titles follow [Conventional Commits](https://www.conventionalcommits.org)
  (`feat(compute): ...`, `fix(fault): ...`, `docs: ...`). PRs are squash-merged, so
  the title becomes the commit and the changelog entry.
- The `squall` module requires nothing beyond the Go standard library, so adding
  it never moves a user's other dependencies.
- A new method or service is tested through the official Go client library for
  that API, so the stand-in is held to what the client actually sends and parses.
  Those tests live in the `sdktest` module, which requires the client libraries
  and points at this checkout with a `replace`. The package's documentation says
  where the stand-in differs from the real API.
- Versions come from the squash-merged PR titles through release-please and stay
  below 1.0: while below 1.0, a `feat` or a breaking change (`feat!`) bumps the
  minor version and a `fix` bumps the patch.
- A stand-in answers what it does not implement with 404, and rejects input it
  does not understand (an unsupported filter, say) with 400 rather than guessing.
- Run `make check` (lint, vulnerability check and tests with the race detector)
  before opening a PR. Changes to request handling include benchmark results
  (`make bench`) against `canary`.
