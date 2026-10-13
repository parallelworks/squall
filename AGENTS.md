# AGENTS.md

Instructions for coding agents working in this repository.

- Read [CONTRIBUTING.md](CONTRIBUTING.md); it applies to agents too.
- This is a public repository. Code, comments, commits and PR text describe use
  cases generically and never mention private systems or other products.
- Write modern, idiomatic Go for the version in `go.mod`: `iter`, `slices`,
  `maps`, `min`/`max`, `for range n`, `errors.AsType`, `new(expr)`, `omitzero`,
  `b.Loop()`, `t.Context()`, `sync.WaitGroup.Go`.
- Default to no comments. Add one only for a constraint or behavior that would
  surprise a reader.
- Tests call the real package through the official client library where one
  exists. Use `t.Context()`, not `context.Background()`, in tests.
- `make check` must pass. Tools are pinned in `tools/go.mod` and run with
  `go tool -modfile=tools/go.mod`.
