package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// watchFleetMinInterval / watchFleetCheckMinInterval floor the refresh cadence so a tiny
// --interval can't busy-spin; --check (a tmux capture-pane per teammate) gets a higher floor.
const (
	watchFleetMinInterval      = 500 * time.Millisecond
	watchFleetCheckMinInterval = 2 * time.Second
)

// newWatchCmd builds `cc-fleet watch` — stream a live text snapshot of the whole fleet (provider
// teammates + one-shot subagent jobs + workflow runs), refreshed on an interval until
// interrupted. It is the cross-lane companion to `workflow watch <id>`: run it in a backgrounded
// shell to surface the fleet in the /tasks panel. INSPECTION-ONLY and key-safe: it prints only field-source-safe
// columns (never a provider reply / job result, never raw pane text — only canonical health). The
// only writes it triggers are the dead-job result memoization any status read (the board,
// `workflow status`, ListJobs) already performs; it never spawns, stops, or tears down anything.
func newWatchCmd() *cobra.Command {
	var (
		interval time.Duration
		timeout  time.Duration
		check    bool
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream the fleet status board (teammates, subagent jobs, workflow runs) as text",
		Long: `Stream a live text snapshot of the whole cc-fleet fleet — provider teammates, one-shot
subagent jobs, and workflow runs — refreshed on an interval until interrupted (Ctrl-C) or
--timeout. Run it in a backgrounded shell to surface the fleet in the /tasks panel.

Read-only: it never prints a provider reply, a job result, or raw pane text — only canonical
status. --check adds a per-teammate pane health scan (slower; a higher minimum interval).`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < watchFleetMinInterval {
				interval = watchFleetMinInterval
			}
			if check && interval < watchFleetCheckMinInterval {
				interval = watchFleetCheckMinInterval
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for {
				fmt.Print(renderFleet(fleetSnapshot(check), time.Now()))
				select {
				case <-ctx.Done():
					return nil // signal / timeout → clean exit (a fleet view has no failure state)
				case <-tick.C:
				}
			}
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Refresh cadence")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Stop after this duration (0 = until interrupted)")
	cmd.Flags().BoolVar(&check, "check", false, "Scan each teammate's pane for health (slower)")
	return cmd
}

// fleetSnap is one tick's assembled fleet state, gathered from the same clean library funcs the
// TUI board uses — no internal/tui import.
type fleetSnap struct {
	teammates []teardown.Teammate
	jobs      []subagent.Result
	runs      []subagent.WorkflowRun
	titles    map[string]string
}

// fleetSnapshot assembles one tick. A teammate-discovery error (e.g. no tmux) degrades to an
// empty teammate section rather than failing the stream.
func fleetSnapshot(check bool) fleetSnap {
	tm, err := teardown.DiscoverTeammates()
	if err != nil {
		tm = nil
	} else {
		if check {
			tm = teardown.AnnotateHealth(tm)
		}
		tm = teardown.AnnotateHidden(tm)
		tm = teardown.AnnotateLeadSession(tm)
	}
	jobs, _ := subagent.ListJobs()
	runs, _ := subagent.ListRuns()
	return fleetSnap{teammates: tm, jobs: jobs, runs: runs, titles: sessiontitle.Resolve(fleetLeadIDs(tm, jobs))}
}

// fleetLeadIDs collects the distinct lead-session ids carried by teammates + jobs, for
// sessiontitle.Resolve (human labels).
func fleetLeadIDs(tm []teardown.Teammate, jobs []subagent.Result) []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, t := range tm {
		add(t.LeadSessionID)
	}
	for _, j := range jobs {
		add(j.LeadSessionID)
	}
	return ids
}

// renderFleet formats one snapshot. Every opaque string is CleanTitle-scrubbed (terminal-injection
// defense); the columns are a strict ALLOWLIST of field-source-safe data — it NEVER prints
// Result.Result (the provider answer), an error message, or raw pane text. Teammate Status/ErrorClass
// come from AnnotateHealth, which returns only canonical classes.
//
// LOAD-BEARING: the jobs allowlist is the ONLY thing keeping a provider answer off stdout — a Result
// from ListJobs for a just-finished --background job still carries the answer in Result.Result (its
// cache, unlike a sync job's, is not answer-stripped). Do NOT add a result/error/preview column to
// the jobs section.
func renderFleet(s fleetSnap, now time.Time) string {
	clean := sessiontitle.CleanTitle
	var b strings.Builder
	fmt.Fprintf(&b, "─── fleet %s ───\n", now.Format("15:04:05"))

	fmt.Fprintf(&b, "teammates (%d):\n", len(s.teammates))
	if len(s.teammates) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, t := range s.teammates {
		line := fmt.Sprintf("  %s/%s  %s/%s  pane=%s  pid=%d",
			clean(t.Team), clean(t.Name), clean(t.Provider), clean(t.Model), clean(t.PaneID), t.PID)
		if t.Status != "" {
			line += "  " + clean(t.Status)
			if t.ErrorClass != "" {
				line += "(" + clean(t.ErrorClass) + ")"
			}
		}
		if t.Hidden {
			line += "  [hidden]"
		}
		if label := s.titles[t.LeadSessionID]; label != "" {
			line += "  ⟵ " + clean(label)
		}
		b.WriteString(line + "\n")
	}

	fmt.Fprintf(&b, "subagent jobs (%d):\n", len(s.jobs))
	if len(s.jobs) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, j := range s.jobs {
		seg := "  "
		if j.Phase != "" {
			seg += clean(j.Phase) + " "
		}
		seg += clean(j.Label) + "  " + clean(j.Provider) + "/" + clean(j.Model) + "  " + clean(j.Status) + "  " + clean(j.JobID)
		b.WriteString(seg + "\n")
	}

	fmt.Fprintf(&b, "workflow runs (%d):\n", len(s.runs))
	if len(s.runs) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, r := range s.runs {
		b.WriteString("  " + clean(r.RunID) + "  " + clean(r.Name) + "  " + clean(r.Status) + "  " + clean(r.StartedAt) + "\n")
	}
	b.WriteString("\n")
	return b.String()
}
