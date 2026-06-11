BIN_DIR    := ./bin
BIN_NAME   := cc-fleet
BIN        := $(BIN_DIR)/$(BIN_NAME)
PKG        := ./cmd/cc-fleet

# Install destinations. Override on the command line if you want different
# locations, e.g.  make install-bin PREFIX=/usr/local/bin
PREFIX     ?= $(HOME)/.local/bin
SKILL_ROOT ?= $(HOME)/.claude/skills

# Cross-compile output
DIST_DIR   := ./dist

# The per-lane skills + the shared docs they link. The plugin ships them bare
# (skills/<lane>, skills/shared); the global install below prefixes the lane dirs
# (cc-fleet-<lane>) to avoid generic-name collisions, keeping `shared` unprefixed
# so the `../shared/...` links resolve in both layouts (sibling dirs).
SKILL_LANES := subagent team workflow

# Repo-local installed-skill copy (gitignored). `skill-sync` mirrors the canonical
# skills/ tree here; `skill-drift-check` fails if they diverge.
LOCAL_SKILLS := .claude/skills

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

# Install the three per-lane skills (prefixed cc-fleet-<lane>) + the shared docs
# they link. Removes the legacy single cc-fleet skill first, so the old router
# can't coexist and compete with the new skills.
install-skill:
	rm -rf $(SKILL_ROOT)/cc-fleet
	@for lane in $(SKILL_LANES); do \
	  install -d $(SKILL_ROOT)/cc-fleet-$$lane; \
	  install -m 0644 skills/$$lane/SKILL.md $(SKILL_ROOT)/cc-fleet-$$lane/SKILL.md; \
	done
	install -d $(SKILL_ROOT)/shared
	install -m 0644 skills/shared/*.md $(SKILL_ROOT)/shared/
	@echo "Installed the per-lane skills (cc-fleet-{$(shell echo $(SKILL_LANES) | tr ' ' ',')}) + shared to $(SKILL_ROOT)/"

# Mirror the canonical skills/ tree into the gitignored repo-local copy (a dev
# convenience for Claude Code working inside the repo).
skill-sync:
	@rm -rf $(LOCAL_SKILLS)
	@for lane in $(SKILL_LANES); do \
	  mkdir -p $(LOCAL_SKILLS)/$$lane; \
	  cp skills/$$lane/SKILL.md $(LOCAL_SKILLS)/$$lane/SKILL.md; \
	done
	@mkdir -p $(LOCAL_SKILLS)/shared
	@cp skills/shared/*.md $(LOCAL_SKILLS)/shared/
	@echo "Synced $(LOCAL_SKILLS)/ from canonical skills/"

# Fail if the repo-local copy has drifted from canonical. NOT a Go unit test:
# $(LOCAL_SKILLS) is gitignored, so a fresh clone / CI has no copy — a hard test
# there would be a false failure. Run `make skill-sync` after editing a skill.
skill-drift-check:
	@for lane in $(SKILL_LANES); do \
	  if [ ! -f $(LOCAL_SKILLS)/$$lane/SKILL.md ]; then \
	    echo "skill-drift-check: $(LOCAL_SKILLS)/$$lane absent (gitignored) — run 'make skill-sync'"; exit 1; \
	  fi; \
	  diff -q skills/$$lane/SKILL.md $(LOCAL_SKILLS)/$$lane/SKILL.md || { echo "skill-drift-check: DRIFT in $$lane — run 'make skill-sync'"; exit 1; }; \
	done
	@diff -rq skills/shared $(LOCAL_SKILLS)/shared \
	  && echo "skill-drift-check: installed copy matches canonical" \
	  || { echo "skill-drift-check: DRIFT in shared — run 'make skill-sync'"; exit 1; }

# Removes the binary, the ccf symlink, and any skills `make install-skill` placed
# (the prefixed per-lane dirs + shared, and the legacy cc-fleet dir if present).
# Config/profile/secret cleanup is the `cc-fleet uninstall` command's job.
uninstall:
	rm -f $(PREFIX)/$(BIN_NAME) $(PREFIX)/ccf
	rm -rf $(SKILL_ROOT)/cc-fleet $(SKILL_ROOT)/shared
	@for lane in $(SKILL_LANES); do rm -rf $(SKILL_ROOT)/cc-fleet-$$lane; done
	@echo "Removed cc-fleet + ccf (+ skill dirs if installed) from $(PREFIX) / $(SKILL_ROOT)"

test:
	go test ./...

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)

smoke: build
	$(BIN) --version

cross-compile:
	@mkdir -p $(DIST_DIR)
	GOOS=linux   GOARCH=amd64 go build -o $(DIST_DIR)/$(BIN_NAME)-linux-amd64      $(PKG)
	GOOS=linux   GOARCH=arm64 go build -o $(DIST_DIR)/$(BIN_NAME)-linux-arm64      $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -o $(DIST_DIR)/$(BIN_NAME)-darwin-amd64     $(PKG)
	GOOS=darwin  GOARCH=arm64 go build -o $(DIST_DIR)/$(BIN_NAME)-darwin-arm64     $(PKG)
	GOOS=windows GOARCH=amd64 go build -o $(DIST_DIR)/$(BIN_NAME)-windows-amd64.exe $(PKG)
	GOOS=windows GOARCH=arm64 go build -o $(DIST_DIR)/$(BIN_NAME)-windows-arm64.exe $(PKG)
	@echo "Built 6 binaries in $(DIST_DIR)/"

# Local fallback for building release tarballs by hand. The canonical release
# path is .goreleaser.yaml run by the release workflow on a tag; keep this for
# offline / dev packaging only. Unix-only — the Windows .zip is goreleaser-only.
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
	  mkdir -p "$$stage/skills/shared"; \
	  for lane in $(SKILL_LANES); do \
	    mkdir -p "$$stage/skills/$$lane"; \
	    cp skills/$$lane/SKILL.md "$$stage/skills/$$lane/SKILL.md"; \
	  done; \
	  cp skills/shared/*.md "$$stage/skills/shared/"; \
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
