.PHONY: kb build test clean install-controller install-cli install-mcp run-controller run-cli proto web-ui web-ui-dev web-ui-build ui-sync ui-ensure hooks ci deb deb-pve-plugin

# Sync the freshly built web UI into ui/dist for go:embed. The directory is
# gitignored and intentionally kept around after builds so plain `go build`
# and `go test ./...` keep working without re-running npm.
#
# .gitkeep is restored after the copy: it is the one tracked file in ui/dist,
# and it is what lets go:embed resolve on a fresh clone. Dropping it would both
# break that and show up as a spurious deletion in `git status` after a build.
ui-sync: web-ui-build
	@echo "Preparing UI for embedding..."
	@rm -rf ui/dist
	@cp -r web-ui/dist ui/
	@touch ui/dist/.gitkeep

# Ensure ui/dist exists so go:embed (ui/ui.go) compiles. Prefers the real
# web-ui build output; falls back to a clearly marked placeholder so Go
# tests can run on machines without Node.js.
ui-ensure:
	@if [ ! -f ui/dist/index.html ]; then \
		if [ -d web-ui/dist ]; then \
			echo "Syncing ui/dist from existing web-ui/dist..."; \
			rm -rf ui/dist && cp -r web-ui/dist ui/; \
		else \
			echo "web-ui/dist not found; writing placeholder ui/dist (run 'make build' for the real UI)"; \
			mkdir -p ui/dist; \
			echo '<!DOCTYPE html><html><body>Haify UI placeholder - run make build to embed the real UI</body></html>' > ui/dist/index.html; \
		fi \
	fi
	@touch ui/dist/.gitkeep

# Build binaries
build: ui-sync
	@echo "Building haify-controller..."
	go build -o bin/haify-controller ./cmd/controller
	@echo "Building haify..."
	go build -o bin/haify ./cmd/cli
	@echo "Building haify-mcp..."
	go build -o bin/haify-mcp ./cmd/mcp
	GOOS=linux go build -o bin/service-ip ./cmd/service-ip
	go build -o bin/csi-controller ./cmd/csi-controller
	go build -o bin/csi-node ./cmd/csi-node
	go build -o bin/haify-docker ./cmd/haify-docker

# Run tests
test: ui-ensure
	go test -v ./...

# Clean build artifacts
clean:
	rm -rf bin/ ui/dist

# Install controller systemd service
install-controller: build
	@echo "Installing haify-controller..."
	sudo mkdir -p /opt/haify/bin /etc/haify
	sudo cp bin/haify-controller bin/service-ip /opt/haify/bin/
	sudo install -m 755 bin/service-ip /usr/local/bin/service-ip
	sudo cp configs/haify-controller.service configs/service-ip@.service /etc/systemd/system/
	sudo cp configs/controller.toml.example /etc/haify/controller.toml.example
	sudo systemctl daemon-reload
	@echo "Controller installed. Copy /etc/haify/controller.toml.example to"
	@echo "/etc/haify/controller.toml, edit it, then run:"
	@echo "  sudo systemctl enable --now haify-controller"

# Install CLI
install-cli: build
	@echo "Installing haify..."
	sudo install -m 755 bin/haify /usr/local/bin/haify
	sudo ln -sf haify /usr/local/bin/haify-cli
	@echo "CLI installed to /usr/local/bin/haify (haify-cli links to it)"

# Install MCP server
install-mcp: build
	@echo "Installing haify-mcp..."
	sudo cp bin/haify-mcp /usr/local/bin/
	@echo "MCP server installed to /usr/local/bin/haify-mcp"

# Debian packages into dist/: haify-controller for linux/amd64 and linux/arm64
# (controller, service-ip, haify, haify-mcp, units) and the haify-pve-plugin Proxmox
# VE storage plugin. Needs dpkg-deb, Go and web-ui/node_modules (npm ci); see
# scripts/build-deb.sh for VERSION, ARCHES and SKIP_UI_BUILD.
deb:
	./scripts/build-deb.sh

# Just the Proxmox VE plugin package, which needs neither Go nor Node.js.
deb-pve-plugin:
	./scripts/build-deb.sh pve-plugin

# Run controller locally
# configs/controller.toml is local and untracked: copy it from the example.
run-controller:
	@test -f configs/controller.toml || { echo "configs/controller.toml not found: cp configs/controller.toml.example configs/controller.toml and edit it" >&2; exit 1; }
	go run ./cmd/controller --config configs/controller.toml

# Run CLI
run-cli:
	go run ./cmd/cli $(ARGS)

# Generate proto files
proto:
	@echo "Generating proto files..."
	./scripts/generate-proto.sh

# Format code
fmt:
	go fmt ./...
	gofmt -s -w .

# Lint
# The --max flags turn off golangci-lint's output truncation (50 per linter,
# 3 per message by default), which otherwise hides most of what it found and
# picks the survivors non-deterministically. Same flags as CI, so `make lint`
# actually predicts the gate.
lint:
	golangci-lint run ./... --max-issues-per-linter=0 --max-same-issues=0

# Point git at the versioned hooks in .githooks (run once per clone). The
# pre-commit hook rejects staged Go files that are not gofmt-clean, which is
# CI's first gate and the one `go test` cannot catch.
hooks:
	@git config core.hooksPath .githooks
	@echo "git hooks installed (core.hooksPath=.githooks); bypass with --no-verify"

# Run the CI pipeline exactly as .github/workflows/ci.yml does, so a red build
# is found here rather than after a push. Note CI checks plain `gofmt -l`, NOT
# the stricter `gofmt -s` that `make fmt` applies.
ci: ui-ensure
	@echo "==> file size"
	@./scripts/check-file-size.sh
	@echo "==> gofmt"
	@unformatted=$$(gofmt -l . | grep -vE '^(vendor|\.claude)/' || true); \
		if [ -n "$$unformatted" ]; then \
			echo "These files are not gofmt-clean:"; echo "$$unformatted"; exit 1; \
		fi
	@echo "==> go vet"
	@go vet ./...
	@echo "==> golangci-lint"
	@golangci-lint run ./... --max-issues-per-linter=0 --max-same-issues=0
	@echo "==> go vet + golangci-lint + go build as linux"
	@# CI runs on Linux. Code built only there (cmd/service-ip, pkg/serviceip)
	@# is invisible to vet and lint on a Mac, so without this pass the local run
	@# is green while CI fails on it.
	@GOOS=linux go vet ./...
	@GOOS=linux golangci-lint run ./... --max-issues-per-linter=0 --max-same-issues=0
	@GOOS=linux go build ./...
	@echo "==> go build"
	@go build ./...
	@echo "==> go test (race, uncached)"
	@go test -race -count=1 ./...
	@echo "==> python tests (libvirt hook)"
	@python3 -m unittest discover -s deploy/libvirt -p 'test_*.py'
	@echo "==> python tests (Cinder driver, Horizon plugin)"
	@# With Django when uv can provide it, so the plugin's templates compile.
	@if command -v uv >/dev/null; then \
		uv run --quiet --with django python -m unittest discover -s deploy/openstack/tests -p 'test_*.py'; \
	else \
		python3 -m unittest discover -s deploy/openstack/tests -p 'test_*.py'; \
	fi
	@echo "==> helm lint (CSI chart)"
	@helm lint --strict deploy/helm/haify-csi --set controller.address=192.0.2.10
	@echo "==> govulncheck"
	@# Scanned with the newest Go, as CI's vuln job is: a standard library
	@# finding is fixed by upgrading Go, and a local Go a patch release behind
	@# would fail here on advisories CI no longer reports. Falls back to the
	@# local Go when go.dev cannot be reached.
	@latest=$$(curl -fsS -m 10 'https://go.dev/VERSION?m=text' 2>/dev/null | head -1); \
		GOTOOLCHAIN=$${latest:-local} go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
	@echo "==> web-ui build"
	@npm --prefix web-ui run build --silent >/dev/null
	@echo "CI pipeline passed"

# Dependencies
deps:
	go mod download
	go mod tidy

# Web UI
web-ui-dev:
	cd web-ui && npm run dev

web-ui-build:
	cd web-ui && npm run build

web-ui-install: web-ui-build
	@echo "Installing web-ui..."
	sudo mkdir -p /opt/haify/www
	sudo cp -r web-ui/dist/* /opt/haify/www/
	@echo "Web UI installed to /opt/haify/www/"

# The shared knowledge base haify-ai attaches read-only on every cluster:
# docs, code graph, CLI reference and DRBD manuals. See ai/kb/build.sh for
# the environment it needs.
kb:
	ai/kb/build.sh dist/kb
