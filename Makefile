PANEL_REPOSITORY ?= sofyan48/CPAHomeUI
PANEL_SOURCE_DIR ?=
PANEL_WORKDIR ?= $(CURDIR)/.tmp/home-management-center
EMBED_STATIC_DIR := internal/managementasset/static
LOCAL_BINARY ?= CLIProxyAPIHome
DOCKER_IMAGE ?= cliproxyapi-home:embedded-local

.PHONY: embed-local panel-assets-local docker-embed-local run-embedded-local run-downloaded-panel-local clean-embedded-local

# Local-only helper. GitHub Actions builds embedded assets independently.
embed-local: panel-assets-local
	go build -o "$(LOCAL_BINARY)" ./cmd/home

docker-embed-local: panel-assets-local
	docker build -t "$(DOCKER_IMAGE)" .

panel-assets-local:
	@command -v bun >/dev/null || { echo "bun is required"; exit 1; }
	@if [ -n "$(PANEL_SOURCE_DIR)" ]; then \
		panel_dir="$(abspath $(PANEL_SOURCE_DIR))"; \
	else \
		command -v gh >/dev/null || { echo "gh is required when PANEL_SOURCE_DIR is empty"; exit 1; }; \
		rm -rf "$(PANEL_WORKDIR)"; \
		mkdir -p "$(dir $(PANEL_WORKDIR))"; \
		gh repo clone "$(PANEL_REPOSITORY)" "$(PANEL_WORKDIR)" -- --depth 1; \
		panel_dir="$(PANEL_WORKDIR)"; \
	fi; \
	cd "$$panel_dir" && bun install --frozen-lockfile && bun run build:embedded; \
	mkdir -p "$(CURDIR)/$(EMBED_STATIC_DIR)"; \
	touch "$(CURDIR)/$(EMBED_STATIC_DIR)/.gitkeep"; \
	find "$(CURDIR)/$(EMBED_STATIC_DIR)" -mindepth 1 ! -name ".gitkeep" -exec rm -rf {} +; \
	cp -R "$$panel_dir/dist/." "$(CURDIR)/$(EMBED_STATIC_DIR)/"; \
	test -s "$(CURDIR)/$(EMBED_STATIC_DIR)/management.html"; \
	test -s "$(CURDIR)/$(EMBED_STATIC_DIR)/user.html"

run-embedded-local: panel-assets-local
	go run ./cmd/home

# Run with previously downloaded release assets without rebuilding the panel.
run-downloaded-panel-local:
	@test -s "$(EMBED_STATIC_DIR)/management.html" || { echo "Missing panel assets in $(EMBED_STATIC_DIR); download the management-panel-static release artifact first"; exit 1; }
	go run ./cmd/home

clean-embedded-local:
	rm -rf "$(PANEL_WORKDIR)"
	rm -rf "$(EMBED_STATIC_DIR)"
	mkdir -p "$(EMBED_STATIC_DIR)"
	touch "$(EMBED_STATIC_DIR)/.gitkeep"
