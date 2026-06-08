package codexproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// codexCLIAuth is the subset of ~/.codex/auth.json that cc-fleet reads. The
// refresh_token is deliberately absent from this struct so it is never even
// deserialized into memory: cc-fleet rides the codex CLI's access token but
// never rotates its chain, so it cannot trip OpenAI's reuse detection.
type codexCLIAuth struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
		IDToken     string `json:"id_token"`
	} `json:"tokens"`
}

// codexCLIAuthPath is the codex CLI's own credential file, opened READ-ONLY.
func codexCLIAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

// cliRideStore is the composite codex credential source. It prefers cc-fleet's own
// login (an explicit `cc-fleet codex login` on the ownStore device-code chain) and
// rides the codex CLI's existing login (~/.codex/auth.json, read-only) only when
// there is no own login. Re-reading ~/.codex on every ride serves the codex CLI's
// latest token for free, without cc-fleet ever refreshing that chain itself.
type cliRideStore struct {
	own *ownStore
}

func newCLIRideStore() (*cliRideStore, error) {
	own, err := newOwnStore()
	if err != nil {
		return nil, err
	}
	return &cliRideStore{own: own}, nil
}

func (s *cliRideStore) token(ctx context.Context) (bearer, error) {
	// cc-fleet's own login is authoritative: an explicit `cc-fleet codex login`
	// wins. Ride the codex CLI's ~/.codex only when there is no own login; with
	// neither, the own store returns ErrReauth.
	if s.own.loggedIn() {
		return s.own.token(ctx)
	}
	if b, ok := readCLIRideToken(); ok {
		return b, nil
	}
	return s.own.token(ctx)
}

// invalidate forwards only to the own chain: a CLI-ride token is generation 0 and
// cc-fleet cannot refresh it (that is the codex CLI's job), so a 401 on it is
// terminal here — the next token() re-reads ~/.codex and falls back if it is gone.
func (s *cliRideStore) invalidate(gen uint64) {
	if gen != 0 {
		s.own.invalidate(gen)
	}
}

// readCLIRideToken reads a still-valid access token from ~/.codex/auth.json
// read-only and returns it as a generation-0 bearer. A missing/corrupt file or an
// expired/near-expiry token returns ok=false so the caller falls back to the own
// chain. The file is never written and the refresh_token is never read.
func readCLIRideToken() (bearer, bool) {
	p, err := codexCLIAuthPath()
	if err != nil {
		return bearer{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return bearer{}, false
	}
	var a codexCLIAuth
	if err := json.Unmarshal(b, &a); err != nil {
		return bearer{}, false // partial/corrupt (codex CLI mid-rotation): unavailable
	}
	access := a.Tokens.AccessToken
	if access == "" {
		return bearer{}, false
	}
	exp, ok := tokenExpiry(access)
	if !ok || !time.Now().Before(exp.Add(-refreshSkew)) {
		return bearer{}, false // expired or near expiry: fall back to the own chain
	}
	account := accountIDFromTokens(&tokens{
		AccessToken: access,
		AccountID:   a.Tokens.AccountID,
		IDToken:     a.Tokens.IDToken,
	})
	return bearer{accessToken: access, accountID: account, generation: 0}, true
}

// CredStatus reports the state of both codex credential sources for
// `cc-fleet codex status` and the TUI's CLI-auth flow.
type CredStatus struct {
	CLIRide  bool   // an unexpired ~/.codex access token is present
	OwnLogin bool   // cc-fleet's own refresh chain is present
	Active   string // the source token() would serve now: "cli-ride", "own", or "none"
	Account  string // masked account id of the active source, if any
}

// StatusReport summarizes both codex credential sources without any refresh or
// network call (it only reads what is already on disk).
func StatusReport() CredStatus {
	var st CredStatus
	if loggedIn, account := LoginStatus(); loggedIn {
		st.OwnLogin = true
		st.Active = "own"
		st.Account = account
	}
	if b, ok := readCLIRideToken(); ok {
		st.CLIRide = true
		if st.Active == "" {
			st.Active = "cli-ride"
			st.Account = redactAccount(b.accountID)
		}
	}
	if st.Active == "" {
		st.Active = "none"
	}
	return st
}
