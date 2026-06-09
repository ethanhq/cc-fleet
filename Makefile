BIN_DIR    := ./bin
BIN_NAME   := cc-fleet
BIN        := $(BIN_DIR)/$(BIN_NAME)
PKG        := ./cmd/cc-fleet

# Install destinations. Override on the command line if you want different
# locations, e.g.  make install-bin PREFIX=/usr/local/bin
PREFIX     ?= $(HOME)/.local/bin
SKILL_DIR  ?= $(HOME)/.claude/skills/cc-fleet

# Cross-compile output
DIST_DIR   := ./dist

# Repo-local installed-skill copy (gitignored). `skill-sync` refreshes it from
# the canonical source; `skill-drift-check` fails if they diverge.
LOCAL_SKILL := .claude/skills/cc-fleet/SKILL.md

.PHONY: build install install-bin install-skill skill-sync skill-drift-check uninstall test clean smoke cross-compile release-archive release-prep version-guard

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN) $(PKG)

# `make install` installs the BINARY only (+ ccf alias). The skill is delivered
# by the cc-fleet plugin (/plugin install) OR explicitly via `make install-skill`
# — installing it both ways would duplicate the skill.
install: install-bin

install-bin: build
	install -d $(PREFIX)
	install -m 0755 $(BIN) $(PREFIX)/$(BIN_NAME)
	ln -sf $(BIN_NAME) $(PREFIX)/ccf
	@echo "Installed to $(PREFIX)/$(BIN_NAME) (+ ccf alias)"

install-skill:
	install -d $(SKILL_DIR)/references
	install -m 0644 skills/cc-fleet/SKILL.md $(SKILL_DIR)/SKILL.md
	install -m 0644 skills/cc-fleet/references/*.md $(SKILL_DIR)/references/
	@echo "Installed skill (+ references) to $(SKILL_DIR)/"

# Refresh the repo-local installed copy from the canonical source. The copy is
# gitignored (a local Claude Code install copy), so this is a dev convenience.
skill-sync:
	@mkdir -p $(dir $(LOCAL_SKILL))references
	@cp skills/cc-fleet/SKILL.md $(LOCAL_SKILL)
	@cp skills/cc-fleet/references/*.md $(dir $(LOCAL_SKILL))references/
	@echo "Synced $(LOCAL_SKILL) (+ references) from canonical skills/cc-fleet/"

# Fail if the repo-local installed copy has drifted from canonical. NOT a Go
# unit test: $(LOCAL_SKILL) is gitignored, so a fresh clone / CI has no copy — a
# hard test there would be a false failure. This target is a local guard; run
# `make skill-sync` after editing the canonical SKILL.
skill-drift-check:
	@if [ ! -f $(LOCAL_SKILL) ]; then \
	  echo "skill-drift-check: $(LOCAL_SKILL) absent (gitignored local copy) — run 'make skill-sync'"; \
	  exit 1; \
	fi
	@diff -q skills/cc-fleet/SKILL.md $(LOCAL_SKILL) \
	  && diff -rq skills/cc-fleet/references $(dir $(LOCAL_SKILL))references \
	  && echo "skill-drift-check: installed copy (SKILL.md + references) matches canonical" \
	  || { echo "skill-drift-check: DRIFT — run 'make skill-sync'"; exit 1; }

# Removes the binary, the ccf symlink, and — if present — the skill that
# `make install-skill` would have placed (rm -f, harmless when absent; note
# `make install` no longer installs the skill). Config/profile/secret cleanup
# is the `cc-fleet uninstall` command's job (it never touches the binary).
uninstall:
	rm -f $(PREFIX)/$(BIN_NAME) $(PREFIX)/ccf
	rm -rf $(SKILL_DIR)
	@echo "Removed cc-fleet + ccf (+ skill dir if installed) from $(PREFIX) / $(SKILL_DIR)"

test:
	go test ./...

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)

smoke: build
	$(BIN) --version

cross-compile:
	@mkdir -p $(DIST_DIR)
	GOOS=linux  GOARCH=amd64 go build -o $(DIST_DIR)/$(BIN_NAME)-linux-amd64  $(PKG)
	GOOS=linux  GOARCH=arm64 go build -o $(DIST_DIR)/$(BIN_NAME)-linux-arm64  $(PKG)
	GOOS=darwin GOARCH=amd64 go build -o $(DIST_DIR)/$(BIN_NAME)-darwin-amd64 $(PKG)
	GOOS=darwin GOARCH=arm64 go build -o $(DIST_DIR)/$(BIN_NAME)-darwin-arm64 $(PKG)
	@echo "Built 4 binaries in $(DIST_DIR)/"

# Local fallback for building release tarballs by hand. The canonical release
# path is .goreleaser.yaml run by the release workflow on a tag; keep this for
# offline / dev packaging only.
#
# Per-platform tarballs: each cc-fleet-<os>-<arch>.tar.gz bundles the prebuilt
# binary (renamed cc-fleet) + the cc-fleet SKILL.md + a copy-binary installer
# (release/install.sh — NO go build) + a short README. Depends on cross-compile;
# the bare dist/cc-fleet-<os>-<arch> binaries stay as dev artifacts. The staging
# tree lives under dist/release/ and is cleaned up.
release-archive: cross-compile
	@set -e; \
	stage_root="$(DIST_DIR)/release"; \
	rm -rf "$$stage_root"; \
	for plat in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do \
	  stage="$$stage_root/$(BIN_NAME)-$$plat"; \
	  mkdir -p "$$stage"; \
	  cp "$(DIST_DIR)/$(BIN_NAME)-$$plat" "$$stage/$(BIN_NAME)"; \
	  chmod +x "$$stage/$(BIN_NAME)"; \
	  cp skills/cc-fleet/SKILL.md "$$stage/SKILL.md"; \
	  mkdir -p "$$stage/references"; \
	  cp skills/cc-fleet/references/*.md "$$stage/references/"; \
	  cp release/install.sh "$$stage/install.sh"; \
	  chmod +x "$$stage/install.sh"; \
	  cp release/README.md "$$stage/README.md"; \
	  tar -czf "$(DIST_DIR)/$(BIN_NAME)-$$plat.tar.gz" -C "$$stage_root" "$(BIN_NAME)-$$plat"; \
	  echo "  packaged $(DIST_DIR)/$(BIN_NAME)-$$plat.tar.gz"; \
	done; \
	rm -rf "$$stage_root"; \
	echo "Built 4 release archives in $(DIST_DIR)/"

# Bump the plugin manifest + npm package version in lockstep before tagging a
# release (VERSION=vX.Y.Z). version-guard asserts these match the tag at release.
release-prep:
	@test -n "$(VERSION)" || { echo "usage: make release-prep VERSION=vX.Y.Z"; exit 1; }
	@v="$(VERSION)"; v="$${v#v}"; \
	  for f in .claude-plugin/plugin.json npm/package.json; do \
	    tmp=$$(mktemp); \
	    sed 's/\("version"[[:space:]]*:[[:space:]]*"\)[^"]*"/\1'"$$v"'"/' "$$f" > "$$tmp" && mv "$$tmp" "$$f"; \
	  done; \
	  echo "release-prep: set plugin.json + npm/package.json to $$v — commit, then tag v$$v"

# Assert plugin.json == npm/package.json == VERSION (the release-time guard, run
# locally). VERSION defaults to the latest git tag.
version-guard:
	@bash scripts/version-guard.sh "$(or $(VERSION),$$(git describe --tags --abbrev=0))"
