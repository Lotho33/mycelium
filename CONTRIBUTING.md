# Contributing

Thanks for your interest in mycelium.

## Scope

mycelium is the **runtime**: HTTP APIs, admin dashboard, HLS proxy, client
auth, scheduler, and the plugin engine. It is deliberately source-agnostic and
ships with no content providers. Anything that knows about a specific site or
service belongs in a **plugin**, not in this repo — see
[docs/plugin-sdk.md](docs/plugin-sdk.md).

Please don't open PRs or issues that add, request, or discuss specific
third-party services, scrapers, or their APIs.

## Development

```bash
go run ./cmd/server        # http://localhost:8000/setup
```

or use the devcontainer under `.devcontainer/` (Go toolchain + protoc pinned).

Before opening a PR:

```bash
gofmt -l .        # must be empty (third_party/ excluded)
go vet ./...
go test ./...
go build ./...
```

CI runs the same checks plus `GOOS=linux/arm64` and `GOOS=windows/amd64`
cross-compiles.

## Style

- Match the surrounding code: comment density, naming, error handling.
- Keep changes focused; unrelated refactors go in their own PR.
- Regenerate proto bindings with `scripts/regen-proto.sh` (needs `protoc` +
  `protoc-gen-go` + `protoc-gen-go-grpc` — the devcontainer has them) and commit
  the `.pb.go` alongside the `.proto` change.

## Commit messages

Short imperative summary; body explaining *why* when it isn't obvious.

## License

By contributing you agree your work is licensed under the repository's MIT
license.
