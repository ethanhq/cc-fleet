package spawn

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
	"github.com/ethanhq/cc-fleet/internal/profile"
	"github.com/ethanhq/cc-fleet/internal/tmux"
	"github.com/ethanhq/cc-fleet/internal/vendorclass"
)

// Test seams. Production code calls the helpers behind these vars; tests can
// swap them to force a writeTeamConfig / ensureInbox failure AFTER the pane is
// created, and assert the cleanup path kills the pane (and, for swarm spawns,
// the private server) instead of leaking it.
var (
	writeTeamConfigFn = WriteTeamConfig
	ensureInboxFn     = EnsureInbox
	// killPaneOnServer is the rollback's pane-kill primitive. It defaults to
	// the package's tmux.NewServer(socket).KillPane, but tests can replace it
	// to record which (socket, paneID) pairs the rollback targeted.
	killPaneOnServer = func(socket, paneID string) error {
		return tmux.NewServer(socket).KillPane(paneID)
	}
	// killServerOnSocket tears down the private swarm server in the rollback's
	// swarm branch. Default = tmux.NewServer(socket).KillServer.
	killServerOnSocket = func(socket string) error {
		return tmux.NewServer(socket).KillServer()
	}
	// reapAgentProcess is the rollback's process-reap primitive: a best-effort
	// kill by agent id. Without it the spawn cleanup would leak the claude
	// process even after killing the pane (a ghost teammate).
	reapAgentProcess = defaultReapAgentProcess
)

// findAgentPIDs returns the pids of processes whose argv carries
// `--agent-id <agentID>`. Cross-platform via procintrospect (Linux /proc,
// darwin ps); best-effort, returning nil on any introspection error. Our own
// pid is skipped. Used only by the spawn-rollback reap.
//
// The match is exact on the argv pair (--agent-id followed by agentID), so it
// can't be fooled by an agent id that happens to be a prefix of another.
func findAgentPIDs(agentID string) []int {
	if agentID == "" {
		return nil
	}
	procs, err := procintrospect.ProcessTable()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var out []int
	for _, p := range procs {
		if p.PID == self {
			continue
		}
		if argvHasAgentID(p.Argv, agentID) {
			out = append(out, p.PID)
		}
	}
	return out
}

// argvHasAgentID reports whether argv contains the flag `--agent-id` immediately
// followed by want.
func argvHasAgentID(argv []string, want string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--agent-id" && argv[i+1] == want {
			return true
		}
	}
	return false
}

// palette is the closed list of teammate colors we cycle through when the
// caller doesn't pin one. These MUST be Claude AgentColorName values, NOT raw
// tmux names: the color goes verbatim into --agent-color so the teammate
// self-colors its TUI, and tmuxColorName maps it to the pane-border color. A
// tmux-only name here (e.g. "magenta") would paint the border but leave the
// teammate's input box unthemed. Auto-pick uses member-count % len(palette).
var palette = []string{"red", "blue", "green", "yellow", "purple", "orange", "pink", "cyan"}

// useSwarm reports whether a spawn should take the out-of-tmux swarm branch
// (a private socket-scoped server) instead of splitting an existing pane. It is
// true ONLY when the caller gave no explicit --target AND is not inside tmux
// (empty $TMUX). Gate on $TMUX — never $TMUX_PANE, never a `tmux display-message`
// probe — and check it BEFORE the $TMUX_PANE / PickAttachedSession fallbacks, so
// an out-of-tmux spawn can never hijack some unrelated attached session belonging
// to another screen/user. An explicit --target always keeps the in-tmux path, so
// reqTarget != "" returns false even outside tmux.
func useSwarm(reqTarget, tmuxEnv string) bool {
	return reqTarget == "" && tmuxEnv == ""
}

// Spawn runs the full spawn pipeline and returns a structured Result.
//
// Returned Result.OK = false carries an ErrorCode + ErrorMsg; the caller writes
// that to stdout (as JSON) and exits 1. We never return Go errors — every
// failure path produces a Result.
func Spawn(req Request) Result {
	// Typed-ID boundary at the spawn entry: defense-in-depth for any future
	// caller (e.g. a programmatic embedder) that bypassed the CLI validators.
	// Empty fields are checked separately below — those envelopes are friendlier
	// than the validator's "empty" wrap — so only run typed construction when a
	// value is present.
	if req.Team != "" {
		if _, err := ids.NewTeamID(req.Team); err != nil {
			return fail(ErrCodeTeamNotFound, err.Error(), req.Vendor, "")
		}
	}
	if req.AgentName != "" {
		if _, err := ids.NewAgentName(req.AgentName); err != nil {
			return fail(ErrCodeUnknownVendor, err.Error(), req.Vendor, "")
		}
	}
	// 1. Load vendor config.
	cfg, err := config.Load()
	if err != nil {
		return fail(ErrCodeUnknownVendor,
			fmt.Sprintf("load vendors.toml: %v", err),
			req.Vendor,
			"Run cc-fleet init to scaffold config")
	}
	v, ok := cfg.Vendors[req.Vendor]
	if !ok {
		return fail(ErrCodeUnknownVendor,
			fmt.Sprintf("vendor %q not in vendors.toml", req.Vendor),
			req.Vendor,
			"Run cc-fleet add "+req.Vendor)
	}
	if !v.Enabled {
		return fail(ErrCodeVendorDisabled,
			fmt.Sprintf("vendor %q is disabled in vendors.toml", req.Vendor),
			req.Vendor,
			"Run cc-fleet edit "+req.Vendor+" --enable")
	}

	// 2. Required request fields.
	if req.AgentName == "" {
		return fail(ErrCodeUnknownVendor, // closest existing code; CLI validates first
			"agent name (--as) is required", req.Vendor, "")
	}
	if req.Team == "" {
		return fail(ErrCodeTeamNotFound,
			"team (--team) is required", req.Vendor, "")
	}

	// 3. Resolve model (capability keyword default/strong/fast → slot id, else a
	//    literal id, "" → default_model).
	model := v.ResolveModel(req.Model)

	// 4. Optional vendor probe (3s GET against models_endpoint, with key).
	//    Skipped for a codex provider: its models endpoint is served by the
	//    lazily-started conversion daemon, so probing here (before 5c starts it)
	//    would always fail — daemon readiness at 5c is the real health signal. An
	//    openai-* provider still probes (its models_endpoint is the real upstream).
	if req.Probe && v.EffectiveProtocol() != config.ProtocolCodexOAuth {
		if res := probeVendor(v); res != nil {
			res.Vendor = req.Vendor
			return *res
		}
	}

	// 5. Resolve the spawn recipe: the user's probed fingerprint if present,
	//    else the bundled default — a fresh install spawns with no
	//    FINGERPRINT_MISSING probe ceremony. LoadOrBundled only errors on a
	//    corrupt EXISTING cache.
	fp, err := fingerprint.LoadOrBundled()
	if err != nil {
		return fail(ErrCodeFingerprintMissing,
			fmt.Sprintf("load fingerprint: %v", err),
			req.Vendor,
			"Existing fingerprint cache is unreadable — remove it or run cc-fleet refresh-fingerprint")
	}
	// 5b. Resolve the binary path live (cached-if-still-on-disk, else ccver) so a
	//     CC upgrade that GC'd the recipe's version-pinned path can't strand the
	//     spawn. MUST run BEFORE any state-mutating step (profile write at step 6,
	//     EnsureTeamDir + SplitWindow inside the team-lock callback at step 8) so a
	//     no-binary situation never leaves a half-built pane behind. STALE here
	//     means "no claude binary at all".
	binPath, err := fingerprint.ResolveBinaryPath(fp)
	if err != nil {
		return fail(ErrCodeFingerprintStale,
			err.Error(),
			req.Vendor,
			"No claude binary found — install Claude Code or check PATH")
	}
	fp.BinaryPath = binPath
	// Shared runtime gate — defence in depth after the dynamic resolution above;
	// the same helper subagent.Run uses, so they can't drift on what counts as
	// "usable now".
	if err := fingerprint.ValidateForRuntime(fp); err != nil {
		return fail(ErrCodeFingerprintStale,
			err.Error(),
			req.Vendor,
			"Skill's self-heal probe re-captures the recipe")
	}
	// The post-spawn settle check runs only when the caller opted in (req.Verify)
	// AND the live CC is newer than the recipe (then a flag/env may have drifted).
	// Short-circuit on req.Verify first so a --no-verify spawn never pays for
	// CurrentVersionExceedsRecipe's ccver probe (which can exec `claude --version`).
	// Computed before the lock, consumed after a successful spawn below.
	runSettle := req.Verify && fingerprint.CurrentVersionExceedsRecipe(fp)

	// 5c. For a codex provider, ensure the conversion daemon is up — after the
	//     fingerprint gate, before the profile write and the tmux split, so a
	//     daemon failure is fail-before-mutation (no profile, no pane).
	if err := ensureVendorProxy(v); err != nil {
		return fail(ErrCodeProxyUnavailable, err.Error(), req.Vendor,
			"Run cc-fleet codex login (and check the codex base_url port is free), then retry")
	}

	// 6. Ensure the per-vendor profile exists (idempotent write).
	profilePath, err := profile.WriteForVendor(v, "")
	if err != nil {
		return fail(ErrCodeUnknownVendor,
			fmt.Sprintf("write profile for %s: %v", req.Vendor, err),
			req.Vendor, "")
	}

	// 7. Resolve the tmux split target. Priority:
	//      1. req.Target (--target) — explicit caller override, highest.
	//      2. $TMUX_PANE — the caller's own pane id, set by tmux whenever
	//         cc-fleet runs inside a pane (e.g. the main Claude session's Bash).
	//         Splitting off this exact pane guarantees the teammate lands beside
	//         the caller, not some other attached session.
	//      3. PickAttachedSession() — fallback when run outside tmux.
	//    tmuxSession is the human-readable session reported back. $TMUX_PANE is
	//    VALIDATED via SessionForPane: a live pane gives its session name and is
	//    used; a stale/dead pane (or a failed lookup) falls through to
	//    PickAttachedSession instead of dead-ending the split on a gone pane.
	target := req.Target
	tmuxSession := target
	swarm := false
	swarmSocket := ""
	if target == "" {
		// Out-of-tmux gate: an empty $TMUX means cc-fleet wasn't launched inside
		// any tmux server, so there's no inherited pane to split into — build a
		// private, persistently-named swarm server instead. This gate MUST sit
		// BEFORE the $TMUX_PANE / PickAttachedSession fallbacks: out of tmux we
		// must never split into some unrelated attached session belonging to a
		// different screen/user. An explicit --target always wins (handled by
		// target != "" above) and keeps the in-tmux path.
		if useSwarm(req.Target, os.Getenv("TMUX")) {
			swarm = true
			swarmSocket = SwarmSocketName(req.Team)
			tmuxSession = tmux.SwarmSessionName
		} else {
			if pane := os.Getenv("TMUX_PANE"); pane != "" {
				if sess, lookupErr := tmux.SessionForPane(pane); lookupErr == nil {
					target = pane
					tmuxSession = sess
				}
			}
			if target == "" {
				picked, err := tmux.PickAttachedSession()
				if err != nil {
					return fail(ErrCodePaneCreationFailed,
						fmt.Sprintf("pick tmux target: %v", err),
						req.Vendor,
						"Open a tmux session first (e.g. tmux new -s 1) and re-run")
				}
				target = picked
				tmuxSession = picked
			}
		}
	}

	// 8. Build per-spawn context. Pane id / color / lead-session-id resolve
	//    inside the lock so concurrent spawns to the same team see a
	//    consistent member count when picking colors.
	color := req.Color
	leadSessionID := req.LeadSessionID
	agentID := req.AgentName + "@" + req.Team
	spawnTime := time.Now().UTC().Format(time.RFC3339)

	var paneID string
	// swarmCreatedServer escapes the lock so the rollback paths know whether THIS
	// spawn created the swarm session. Only the session-creating (first) member's
	// rollback may kill the whole swarm server + clear the team socket; a later
	// member's rollback must leave the running first member alone.
	var swarmCreatedServer bool
	// permSource escapes the lock so the success Result can report where the
	// teammate's permission flags came from.
	var permSource string
	lockErr := config.WithTeamLock(req.Team, func() error {
		// 8a. Ensure team dir + config.json. ErrTeamNotFound is fine if
		//     AutoTeam is true — we create the team config below.
		tc, loadErr := LoadTeamConfig(req.Team)
		if errors.Is(loadErr, ErrTeamNotFound) {
			if !req.AutoTeam {
				return fmt.Errorf("%w: team %q does not exist", ErrTeamNotFound, req.Team)
			}
			if err := EnsureTeamDir(req.Team); err != nil {
				return err
			}
			tc = &TeamConfig{Members: nil, Raw: map[string]any{}}
		} else if loadErr != nil {
			return loadErr
		} else {
			if err := EnsureTeamDir(req.Team); err != nil {
				return err
			}
		}

		// 8b. Resolve leadSessionId: explicit > team config > new UUID.
		//     We persist the chosen value into tc.LeadSessionID iff the team
		//     config doesn't already pin one — that way an explicit caller-
		//     provided id (or auto-generated UUID) sticks for future spawns
		//     in the same team, but we never overwrite a value an existing
		//     team config already established.
		if leadSessionID == "" {
			leadSessionID = tc.LeadSessionID
		}
		if leadSessionID == "" {
			if !req.AutoTeam {
				return fmt.Errorf("no lead session id and AutoTeam disabled")
			}
			leadSessionID = uuid.NewString()
		}
		if tc.LeadSessionID == "" {
			tc.LeadSessionID = leadSessionID
		}

		// 8c. Reject a duplicate (team, name) BEFORE spending the tmux split.
		// Splitting first and noticing the duplicate after would leak an
		// unrecorded pane + claude process that teardown could never find. Catch
		// it here so the caller gets an actionable DUPLICATE_NAME envelope and no
		// resources are wasted.
		for _, existing := range tc.Members {
			if existing.Name == req.AgentName {
				return fmt.Errorf("DUPLICATE_NAME: agent %q already exists in team %q",
					req.AgentName, req.Team)
			}
		}

		// 8c-color. Auto-pick color from member count when caller didn't pin one.
		if color == "" {
			color = palette[len(tc.Members)%len(palette)]
		}

		// 8d. Build the spawn command inside the lock so the pane id we record in
		//     the member entry corresponds to the pane we just split.
		//
		//     Resolve the teammate's permission flags from the lead session's
		//     startup intent (or a manual override). source == "frozen-template"
		//     means we found no validated lead.
		//
		//     SECURITY: ALWAYS strip the captured permission flags, even on
		//     frozen-template — the bundled recipe carries
		//     --dangerously-skip-permissions, so NOT stripping on the fallback
		//     would let an undetectable-lead spawn silently inherit FULL bypass.
		//     We strip unconditionally and append only `inherited` (nil on
		//     frozen-template → claude's safe interactive default); an operator
		//     who wants bypass passes it explicitly (source=manual).
		inherited, source := inheritPermissionFlags(req.PermissionModeOverride)
		permSource = source
		spawnCmd, buildErr := buildSpawnCommand(fp, fingerprint.SpawnContext{
			Name:          req.AgentName,
			Team:          req.Team,
			Color:         color,
			LeadSessionID: leadSessionID,
		}, profilePath, model, inherited, true)
		if buildErr != nil {
			return buildErr
		}

		// 8e. tmux split-window. This is the long pole of the spawn — once
		//     we have a pane id we know the process is launching. The split and
		//     its main-vertical reflow race at the tmux-SERVER level against
		//     spawns from OTHER teams into the same window (the per-team lock we
		//     hold does NOT serialize those — different inodes), so guard just
		//     this section with the global server lock. Ordering: team-lock
		//     outer, server-lock inner — no cycle, no deadlock. The swarm branch
		//     reuses WithServerLock too: although a per-team socket only races
		//     same-team spawns (already serialized by WithTeamLock), reusing the
		//     one global lock keeps the locking model uniform at negligible cost
		//     (cc-fleet's concurrency is low).
		if splitErr := config.WithServerLock(func() error {
			var e error
			if swarm {
				paneID, swarmCreatedServer, e = tmux.NewServer(swarmSocket).SpawnSwarm(spawnCmd, color, req.AgentName)
			} else {
				paneID, e = tmux.SplitWindow(target, "h", spawnCmd, color, req.AgentName)
			}
			return e
		}); splitErr != nil {
			return fmt.Errorf("split-window: %w", splitErr)
		}

		// Persist the swarm socket name into the team config (under Raw) so a
		// later teardown / ps can find the private server and reap it — without
		// this, the swarm panes + server leak silently.
		if swarm {
			tc.SetTmuxSocket(swarmSocket)
		}

		// 8f. Register member + persist team config. Write the full Member shape
		//     (color, agentType, backend, isActive, etc.) so config.json stays
		//     well-formed for its file-based consumers — chiefly teammates
		//     discovering each other. NOTE: these fields do NOT surface the
		//     teammate in the leader's UI (that renders from the leader's
		//     in-memory state, not this array, so an externally-spawned teammate
		//     won't appear there regardless). The teammate self-colors its TUI
		//     from the --agent-color we pass at spawn, not from this row.
		cwd, _ := os.Getwd() // best-effort; empty is fine
		// Record the member's per-pane socket so a later teardown / hide / show /
		// capture can scope tmux ops to the right server without relying on the
		// team-level legacy field. For in-tmux spawns memberSocket stays ""
		// (omitempty preserves byte-identity with old configs).
		memberSocket := ""
		if swarm {
			memberSocket = swarmSocket
		}
		// Record this spawn's parent Claude session per member so a re-used team
		// with a different caller can map each member to its true lead. Team-level
		// tc.LeadSessionID is also populated above (backward compatibility);
		// AnnotateLeadSession prefers per-member and falls back to team-level.
		member := Member{
			AgentID:       agentID,
			Name:          req.AgentName,
			Color:         color,
			AgentType:     "general-purpose",
			Model:         model,
			JoinedAt:      time.Now().UnixMilli(),
			TmuxPaneID:    paneID,
			Cwd:           cwd,
			Subscriptions: []string{},
			BackendType:   "tmux",
			IsActive:      true,
			Socket:        memberSocket,
			LeadSessionID: leadSessionID,
		}
		// Duplicate detection already ran (step 8c), so appending is unconditional.
		tc.Members = append(tc.Members, member)
		// Post-split rollback. Pane + claude process are live now. If
		// WriteTeamConfig or EnsureInbox fails, the team has no record of the pane
		// → teardown can't find it → vendor key keeps burning. Undo here: KillPane
		// on the right server (default for in-tmux, the swarm socket for swarm),
		// KillServer for swarm, and reap any reparented claude process by agent id.
		// The lock is still held so no concurrent spawn can grab the same pane id.
		if err := writeTeamConfigFn(req.Team, tc); err != nil {
			rollbackPane(paneID, swarm, swarmSocket, agentID, swarmCreatedServer)
			return fmt.Errorf("write team config: %w", err)
		}

		// 8g. Pre-create the inbox file so the new teammate's first
		//     SendMessage doesn't race against an empty path.
		if err := ensureInboxFn(req.Team, req.AgentName); err != nil {
			rollbackPane(paneID, swarm, swarmSocket, agentID, swarmCreatedServer)
			// WriteTeamConfig already persisted the new member + (swarm)
			// tmuxSocket to disk. rollbackPane only undoes live pane / process
			// state; without also undoing the durable config the next retry hits
			// the pre-split duplicate check (step 8c) and can never succeed.
			// Still holding the team lock, so this trim + rewrite is race-free.
			tc.Members = tc.Members[:len(tc.Members)-1]
			// Only clear the socket when this spawn created the swarm server
			// (first member). A later member's failure must not erase the socket
			// the surviving first member still needs.
			if swarm && swarmCreatedServer {
				tc.SetTmuxSocket("")
			}
			if werr := writeTeamConfigFn(req.Team, tc); werr != nil {
				// Best-effort: the primary error (inbox failure) is what the
				// caller cares about; a follow-up rewrite failure just means the
				// next retry will surface DUPLICATE_NAME, which is recoverable.
				fmt.Fprintf(os.Stderr, "spawn: rollback rewrite team config failed: %v\n", werr)
			}
			return fmt.Errorf("ensure inbox: %w", err)
		}
		return nil
	})

	if lockErr != nil {
		// Categorise lock-region errors back into result codes.
		switch {
		case errors.Is(lockErr, ErrTeamNotFound):
			return fail(ErrCodeTeamNotFound, lockErr.Error(), req.Vendor,
				"Run with --auto-team or create the team first")
		case strings.HasPrefix(lockErr.Error(), "DUPLICATE_NAME"):
			// Pre-split duplicate detection — no resources spent.
			return fail(ErrCodeDuplicateName, lockErr.Error(), req.Vendor,
				"Pick a fresh --as name or `cc-fleet teardown` the old teammate first")
		case strings.Contains(lockErr.Error(), "no lead session"):
			return fail(ErrCodeNoLeadSession, lockErr.Error(), req.Vendor,
				"Pass --lead-session-id or enable --auto-team")
		case strings.Contains(lockErr.Error(), "split-window"):
			return fail(ErrCodePaneCreationFailed, lockErr.Error(), req.Vendor,
				"Check tmux target exists and is attached")
		default:
			return fail(ErrCodePaneCreationFailed,
				fmt.Sprintf("spawn pipeline: %v", lockErr),
				req.Vendor, "")
		}
	}

	// Post-spawn settle gate. The pane + member + inbox are committed now;
	// confirm the teammate process actually came up instead of exiting on a
	// rejected flag. runSettle already folds in req.Verify (CLI default-on,
	// --no-verify off) AND "live CC newer than the recipe", so a matched version
	// never pays the latency. A fast exit rolls the whole spawn back so no
	// half-built pane / config row leaks, then surfaces SPAWN_DID_NOT_SETTLE.
	settleSocket := ""
	if swarm {
		settleSocket = swarmSocket
	}
	if runSettle && !settleOK(settleSocket, paneID) {
		rollbackSpawnedMember(req.Team, req.AgentName, paneID, swarm, swarmSocket, agentID, swarmCreatedServer)
		return fail(ErrCodeSpawnDidNotSettle,
			fmt.Sprintf("teammate %s exited during startup — likely a spawn-recipe mismatch on a Claude Code newer than the bundled recipe (%s)",
				agentID, fingerprint.BundledVersion),
			req.Vendor,
			"Run the skill's self-heal probe to capture the current recipe, then retry the spawn")
	}

	res := Result{
		OK:                    true,
		AgentID:               agentID,
		Name:                  req.AgentName,
		Team:                  req.Team,
		PaneID:                paneID,
		TmuxSession:           tmuxSession,
		Model:                 model,
		BaseURL:               v.BaseURL,
		Color:                 color,
		SpawnTime:             spawnTime,
		PermissionInheritance: permSource,
	}
	if swarm {
		// Out-of-tmux: surface the socket + a ready-to-run attach line. The line
		// goes to stderr (not stdout) so it never pollutes the --json envelope or
		// the plain success line; the same data is in res for JSON consumers.
		res.TmuxSocket = swarmSocket
		res.AttachCommand = fmt.Sprintf("tmux -L %s attach -t %s", swarmSocket, tmux.SwarmSessionName)
		fmt.Fprintf(os.Stderr, "swarm teammate ready (started outside tmux) — attach with:\n  %s\n", res.AttachCommand)
	}
	return res
}

// rollbackPane is the post-split cleanup path. After SplitWindow / SpawnSwarm
// returned a live paneID but a later state-write step (WriteTeamConfig /
// EnsureInbox) failed, the pane + its claude process are orphaned — no team
// config records the pane, so teardown will never find it, and the vendor key
// keeps billing. This function undoes the pane creation:
//
//  1. KillPane on the correct server — default tmux for in-tmux spawns, the
//     private swarm socket for swarm spawns. KillPane swallows "can't find
//     pane" so it's idempotent. ALWAYS runs.
//  2. For swarm spawns, KillServer on that socket ONLY when createdServer is
//     true — i.e. this spawn created the swarm session, so it's the first (and
//     only) member and tearing down the whole server orphans nothing. A LATER
//     teammate's failed spawn must NOT kill the server — that would take down
//     the already-running first member. createdServer is false for those, so we
//     only KillPane the failed pane.
//  3. Best-effort process reap by agent id — claude reparents to init if its
//     pane dies first, and a reparented process keeps burning vendor quota.
//
// best-effort: each step's failure is ignored. The point is to free as much as
// we can; the caller already has a real error to surface, the rollback's job
// is just to not make things worse.
func rollbackPane(paneID string, swarm bool, swarmSocket, agentID string, createdServer bool) {
	if paneID == "" {
		return
	}
	socket := ""
	if swarm {
		socket = swarmSocket
	}
	_ = killPaneOnServer(socket, paneID)
	if swarm && swarmSocket != "" && createdServer {
		_ = killServerOnSocket(swarmSocket)
	}
	reapAgentProcess(agentID)
}

// settleWindow is how long the post-spawn settle check waits for a freshly
// spawned teammate to either keep running or exit. A recipe-mismatch crash
// exits well within this; a healthy claude is still alive at the end.
const settleWindow = 2 * time.Second

// settleOK reports whether the freshly-spawned teammate's pane (paneID on the
// server named by socket) is still alive after settleWindow — i.e. it survived
// startup rather than exiting on a rejected flag. It is pane-based, NOT
// /proc-based: a crashed teammate has its pane closed by tmux (remain-on-exit
// off), so PaneExists is the cross-platform liveness signal that works
// identically on Linux and macOS (where there is no /proc). A package var so
// tests stub the outcome without launching a real process or a tmux server.
var settleOK = func(socket, paneID string) bool {
	time.Sleep(settleWindow)
	return tmux.NewServer(socket).PaneExists(paneID)
}

// ensureVendorProxy ensures the codex conversion daemon for a codex provider
// (a no-op for every other vendor). A package var so tests can stub it without
// launching a real daemon process.
var ensureVendorProxy = codexproxy.EnsureForVendor

// rollbackSpawnedMember undoes a FULLY committed spawn (pane + config member +
// inbox) when the post-spawn settle check fails. Unlike rollbackPane — which
// runs inside the still-held team lock on an earlier failure — this runs after
// the lock released, so it re-acquires the team lock to trim the member row.
// Every step is best-effort: the caller already has SPAWN_DID_NOT_SETTLE to
// surface; rollback's only job is to not leak a pane / process / config row.
func rollbackSpawnedMember(team, name, paneID string, swarm bool, swarmSocket, agentID string, createdServer bool) {
	rollbackPane(paneID, swarm, swarmSocket, agentID, createdServer)
	_ = config.WithTeamLock(team, func() error {
		tc, err := LoadTeamConfig(team)
		if err != nil {
			return nil
		}
		kept := tc.Members[:0]
		for _, m := range tc.Members {
			if m.AgentID != agentID {
				kept = append(kept, m)
			}
		}
		tc.Members = kept
		// Only clear the team socket when THIS spawn created the swarm server. A
		// later member's rollback must leave the socket (and the running first
		// member's server) intact — clearing it would strand the surviving
		// member's pane behind a forgotten socket.
		if swarm && createdServer {
			tc.SetTmuxSocket("")
		}
		_ = writeTeamConfigFn(team, tc)
		return nil
	})
	if p, err := InboxPath(team, name); err == nil {
		_ = os.Remove(p)
	}
}

// probeVendor is a thin wrapper over vendorclass.Reachability (the shared
// classifier that also backs cc-fleet subagent --probe). It returns a non-nil
// *Result only when the spawn should be blocked; nil lets the spawn proceed. A
// non-blocking warning (e.g. a 5xx, "vendor reachable but unhappy") is printed
// to stderr. The probe's Code values (VENDOR_UNREACHABLE / KEY_INVALID) equal
// this package's ErrCode* consts.
func probeVendor(v *config.Vendor) *Result {
	p := vendorclass.Reachability(v)
	if p.Warn != "" {
		fmt.Fprint(os.Stderr, p.Warn)
	}
	if !p.Block {
		return nil
	}
	r := fail(p.Code, p.Msg, "", p.Suggestion)
	return &r
}

// buildSpawnCommand assembles the shell-quoted single string that
// tmux.SplitWindow runs in the new pane. Layout:
//
//	env -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN \
//	  CLAUDECODE=1 CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 \
//	  <binary> <fingerprint flags...> --settings <profile> --model <model>
//
// Every token is shell-quoted via tmux.Quote() so vendor-supplied strings
// can't escape into shell metacharacters. The CLAUDECODE/teams env vars come
// straight from fingerprint.Env to match whatever the probe captured.
//
// The leading `env -u` unsets the main session's Anthropic credentials before
// launching the teammate. A new tmux pane inherits the tmux server's
// environment, so a main session running in ANTHROPIC_API_KEY mode (rather
// than OAuth) would otherwise leak its real Anthropic key into the vendor
// teammate — at best an undefined precedence clash with the profile's
// apiKeyHelper, at worst the main key being sent to the vendor's endpoint.
// Vendor auth must come solely from the --settings profile's apiKeyHelper.
//
// inherited carries the permission flags the teammate should adopt from the lead
// session (or a manual override); stripPerms says whether to first remove any
// permission flags the fingerprint froze at capture time. Production passes
// stripPerms=true UNCONDITIONALLY — even on the frozen-template fallback — so a
// captured --dangerously-skip-permissions never silently survives into a spawn.
// stripPerms is kept as a parameter only so tests can exercise the raw,
// unstripped template shape; when false the captured flags pass through verbatim.
func buildSpawnCommand(fp *fingerprint.Fingerprint, ctx fingerprint.SpawnContext, profilePath, model string, inherited []string, stripPerms bool) (string, error) {
	if fp == nil {
		return "", errors.New("nil fingerprint")
	}
	if fp.BinaryPath == "" {
		return "", errors.New("fingerprint missing binary_path")
	}

	parts := []string{"env", "-u", "ANTHROPIC_API_KEY", "-u", "ANTHROPIC_AUTH_TOKEN"}
	// The provider profile owns model/effort selection; unset any value the
	// launching shell exported so it can't override the profile (mirrors childenv
	// on the subagent/run path — one shared key list, no drift).
	for _, k := range childenv.ModelEnvKeys {
		parts = append(parts, "-u", k)
	}
	// Sort env keys for deterministic output (helps tests + diffs).
	for _, k := range sortedKeys(fp.Env) {
		parts = append(parts, tmux.Quote(k+"="+fp.Env[k]))
	}
	parts = append(parts, tmux.Quote(fp.BinaryPath))

	applied := fingerprint.Apply(fp, ctx)
	if stripPerms {
		applied = stripPermissionFlags(applied)
	}
	for _, flag := range applied {
		parts = append(parts, tmux.Quote(flag))
	}
	for _, flag := range inherited {
		parts = append(parts, tmux.Quote(flag))
	}
	parts = append(parts,
		"--settings", tmux.Quote(profilePath),
		"--model", tmux.Quote(model),
	)

	return strings.Join(parts, " "), nil
}

// sortedKeys returns m's keys in lexical order for deterministic iteration.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple insertion sort — tiny n (≤ allowlist size), no need for sort import.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}
