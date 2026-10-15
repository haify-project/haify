# Repository Guidelines

Haify is a Software Defined Storage controller written in Go, built on DRBD and LVM/ZFS. It consists of a gRPC controller (`haify-controller`), a CLI (`haify`, installed with an `haify-cli` link), a Kubernetes CSI driver, an MCP server (`haify-mcp`) and an AI Copilot backend (`haify-ai`). `CLAUDE.md` has the architecture and the gateway package's layout.

## Project Structure

```
haify/
├── cmd/              # Entry points: controller, cli, mcp, haify-csi,
│                     #   service-ip (Linux only), haify-ai (its own Go module)
├── pkg/
│   ├── controller/   # Controller and gRPC server: pools, resources, snapshots, nodes, HA, Self-HA, WAN, backups
│   ├── deployment/   # Command execution on storage nodes over SSH (wraps dispatch)
│   ├── gateway/      # NFS / iSCSI / NVMe-oF gateways (drbd-reactor promoter configs)
│   ├── database/     # BoltDB persistence
│   ├── config/       # controller.toml loading (viper)
│   ├── client/       # gRPC client used by the CLI, haify-mcp and CSI
│   ├── mcpserver/    # MCP tools and runbooks (runbooks/*.md, embedded)
│   ├── mcpauth/      # Tokens and OAuth for `haify-mcp serve`
│   ├── csi/          # Kubernetes CSI driver
│   ├── k8sapp/       # Databases on Kubernetes backed by Haify volumes (`haify-mcp k8s`, haify_k8s_* tools)
│   ├── alert/        # Health detector: cluster state to events
│   ├── event/        # Notification bus, history, webhook delivery
│   ├── triage/       # Turns recorded events into a short problem list
│   ├── inspect/      # Cluster inspection checks behind `haify inspect`
│   ├── backup/       # Off-cluster backups (S3, SMB, WebDAV)
│   ├── drbdtls/      # CA for encrypted DRBD replication
│   ├── wanproxy/     # haify-proxy pair for WAN replication
│   ├── serviceip/    # Floating IP and gratuitous ARP for service-ip
│   ├── rbac/         # Casbin-backed authorization
│   ├── metrics/      # Prometheus metrics
│   ├── logbuf/       # In-memory buffer of recent controller log lines
│   └── util/         # Size parsing helpers
├── api/proto/v1/     # gRPC protobuf definitions
├── web-ui/           # React + TypeScript dashboard (Rsbuild, shadcn/ui, Tailwind)
├── ui/               # go:embed wrapper for the built web UI (ui/dist)
├── configs/          # controller.toml.example and systemd units
├── deploy/           # k8s (CSI manifests), monitoring (Prometheus/Grafana), proxmox (storage plugin)
├── ai/               # Knowledge base build for haify-ai
├── scripts/          # Proto generation, deployment, file-size check, CSI smoke test
└── docs/             # User guide, deployment guide, node prerequisites, MCP, design notes
```

Tests live next to the code as `*_test.go`.

## Build, Test, and Development Commands

```bash
make build          # web UI build + sync, then bin/haify-controller, bin/haify, bin/haify-mcp,
                    #   bin/service-ip (GOOS=linux), bin/haify-csi
make test           # go test -v ./...
make ci             # what CI runs: file size, gofmt -l, vet, golangci-lint (also as GOOS=linux),
                    #   build, go test -race, govulncheck, web-ui build
make hooks          # install the pre-commit hook that rejects staged Go files that are not gofmt-clean
make fmt            # go fmt ./... and gofmt -s -w .
make lint           # golangci-lint with output truncation turned off
make proto          # regenerate gRPC code from api/proto/v1/
make clean          # remove bin/ and ui/dist

# haify-ai is a separate module and not part of make build
cd cmd/haify-ai && go build -o ../../bin/haify-ai .

# Run locally (copy configs/controller.toml.example to configs/controller.toml first)
make run-controller # go run ./cmd/controller --config configs/controller.toml
make run-cli ARGS="pool list"

# Web UI
cd web-ui && npm run dev    # dev server
make ui-sync                # npm run build, then copy web-ui/dist into ui/dist for go:embed
```

`make test` and `make ci` run `make ui-ensure` first, which writes a placeholder `ui/dist` when the web UI has not been built, so Go tests run without Node.js. `make ci` still builds the web UI at the end.

Run `make ci` before pushing. CI checks plain `gofmt -l`, not the stricter `gofmt -s` that `make fmt` applies. `//go:build linux` code (`cmd/service-ip`, `pkg/serviceip`) is only vetted and linted by the GOOS=linux pass.

## Coding Style

- Go: gofmt formatting; `make lint` must be clean.
- File size: every hand-written source file (`.go`, `.ts`, `.tsx`, `.pm`, `.sh`, `.py`), tests included, stays under 600 lines; `scripts/check-file-size.sh` enforces it in `make ci`. Generated protobuf code is exempt. Split by responsibility, not into `_part2` files.
- No TODO comments in production code.
- Errors carry context: `fmt.Errorf("operation: %w", err)`. Logging is structured `zap`.
- Proto: snake_case fields; RPCs are `PascalCase` verb+noun (e.g. `CreateResource`).
- Web UI: TypeScript strict mode; components in `web-ui/src/components/`, pages in `web-ui/src/pages/`, API calls in `web-ui/src/services/`.

## Testing Guidelines

- Standard `testing`; most packages also use `testify/assert` and `testify/require`.
- Test files sit next to the file under test: `gateway.go` → `gateway_test.go`.
- Test names say the behaviour checked, e.g. `TestRunbooksOnlyNameToolsThatExist`.
- Single package: `go test -v ./pkg/controller/`. Single test: `go test -v ./pkg/gateway -run TestGenerateNFSGatewayConfig`.
- `scripts/csi-e2e.sh` is a CSI smoke test against a live Kubernetes cluster with the driver installed.
- MCP runbooks are parsed and checked by `pkg/mcpserver/runbooks_test.go`: each file needs a `# Title` line, a summary line and `Needs: operate|admin`, and may only name tools that exist.

## Commits and Pull Requests

- Conventional Commits: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `perf`, `ci`, with the affected package or subsystem as scope, e.g. `fix(csi): ...`.
- The commit message is the subject line only, no body.
- No `Co-Authored-By` lines and no "Generated with" or similar attribution in commits, PRs, issues, comments or docs.
- PR description: only `Closes #N` (several: `Closes #N. Closes #M.`). Never empty.

## Configuration and Secrets

- The controller config is `/etc/haify/controller.toml`; start from `configs/controller.toml.example`. Default gRPC port: 3374.
- API tokens resolve in order: `--token` flag → `HAIFY_TOKEN` env → `~/.haify/token` → `/etc/haify/token`.
- Never commit real tokens, SSH keys or credentials.
