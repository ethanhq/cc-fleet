package fingerprint

import "strings"

// SpawnContext supplies the per-spawn values that get substituted into a
// Fingerprint's FlagsTemplate. The field set matches the placeholders
// templatize() introduces in capture.go.
type SpawnContext struct {
	Name          string
	Team          string
	Color         string
	LeadSessionID string
}

// placeholders is the substitution table applied to every flag token in
// FlagsTemplate. Order does not matter — each value is a literal token
// produced by templatize(), so there's no risk of overlap.
//
// Note: {cc_version} is intentionally absent — it lives only at the top level
// of the Fingerprint, not inside the flag list.
func (c SpawnContext) placeholders() map[string]string {
	return map[string]string{
		"{name}@{team}":     c.Name + "@" + c.Team,
		"{name}":            c.Name,
		"{team}":            c.Team,
		"{color}":           c.Color,
		"{lead_session_id}": c.LeadSessionID,
	}
}

// Apply substitutes placeholders in fp.FlagsTemplate using ctx and returns a
// fresh []string ready to hand to exec.Command (minus the binary path — the
// caller prepends fp.BinaryPath, plus any cc-fleet-side additions like
// --settings <profile> and --model <provider-model-id>).
//
// A nil fp returns nil. Tokens with no matching placeholder pass through
// verbatim — that's intentional so unknown flags (e.g. --agent-type
// general-purpose) survive a round-trip.
func Apply(fp *Fingerprint, ctx SpawnContext) []string {
	if fp == nil {
		return nil
	}
	repl := ctx.placeholders()
	out := make([]string, len(fp.FlagsTemplate))
	for i, tok := range fp.FlagsTemplate {
		out[i] = substitute(tok, repl)
	}
	return out
}

// substitute applies the placeholder table to a single token. We check the
// full-token match first (covers {name}@{team}) and only fall through to
// per-fragment ReplaceAll for the simple atoms, so the composite
// {name}@{team} stays atomic instead of getting double-substituted.
func substitute(tok string, repl map[string]string) string {
	if v, ok := repl[tok]; ok {
		return v
	}
	// Per-fragment fallback for tokens that don't match the table exactly.
	// In practice the captured tokens are always one of the table keys, but
	// being defensive keeps the helper simple if templatize() grows.
	out := tok
	for k, v := range repl {
		if !strings.ContainsRune(k, '{') || !strings.ContainsRune(k, '}') {
			continue
		}
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}
