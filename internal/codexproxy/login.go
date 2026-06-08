package codexproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// RiskNotice is the consent text shown before a device-code login (CLI and TUI):
// reusing a ChatGPT subscription outside the codex CLI is unofficial, and the risk
// lives at OpenAI's account level — cc-fleet's independent token chain can't remove it.
const RiskNotice = `Reusing a ChatGPT subscription outside the codex CLI is unofficial and may
violate OpenAI's terms of use; the account could be rate-limited or banned.
cc-fleet keeps its own login and never writes or refreshes ~/.codex (it only reads
the codex CLI's token), but that does not remove the account-level risk.`

// Login runs an interactive OAuth device-code login and persists an independent
// token chain to cc-fleet's own store (never touching ~/.codex). It prints the
// verification URL + user code to out and polls until the user authorizes.
func Login(ctx context.Context, out io.Writer) error {
	store, err := newOwnStore()
	if err != nil {
		return err
	}
	oc := newOAuthClient()
	dc, err := oc.startDeviceLogin(ctx, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Open %s and enter code: %s\n", dc.verifyURL, dc.userCode)
	fmt.Fprintln(out, "Waiting for authorization...")

	for time.Now().Before(dc.expiresAt) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dc.interval):
		}
		tk, err := oc.pollDeviceLogin(ctx, dc)
		if errors.Is(err, errAuthPending) {
			continue
		}
		if err != nil {
			return err
		}
		if err := store.setLogin(tk); err != nil {
			return err
		}
		fmt.Fprintf(out, "Logged in (account %s).\n", redactAccount(tk.AccountID))
		return nil
	}
	return errors.New("device code expired before authorization")
}

// LoginSession is a two-phase device-code login an interactive caller (the TUI)
// drives: Begin returns the URL + user code to display, then Poll is called on an
// interval until it reports done. Like Login it only ever touches cc-fleet's own
// token chain, never ~/.codex.
type LoginSession struct {
	VerifyURL string
	UserCode  string

	dc    *deviceCode
	store *ownStore
	oc    *oauthClient
}

// BeginDeviceLogin starts a device-code flow and returns the session to display +
// poll. The caller must have already obtained consent (subscription reuse risk).
func BeginDeviceLogin(ctx context.Context) (*LoginSession, error) {
	store, err := newOwnStore()
	if err != nil {
		return nil, err
	}
	oc := newOAuthClient()
	dc, err := oc.startDeviceLogin(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	return &LoginSession{VerifyURL: dc.verifyURL, UserCode: dc.userCode, dc: dc, store: store, oc: oc}, nil
}

// Poll performs one authorization poll. done=false with a nil error means the
// user has not authorized yet (poll again after Interval); done=true persists the
// new login. An expired code or a hard failure returns an error.
func (s *LoginSession) Poll(ctx context.Context) (done bool, err error) {
	if time.Now().After(s.dc.expiresAt) {
		return false, errors.New("device code expired; restart login")
	}
	tk, err := s.oc.pollDeviceLogin(ctx, s.dc)
	if errors.Is(err, errAuthPending) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.store.setLogin(tk); err != nil {
		return false, err
	}
	return true, nil
}

// Interval is the minimum delay between Poll calls.
func (s *LoginSession) Interval() time.Duration { return s.dc.interval }

// LoginStatus reports whether cc-fleet has an independent codex login.
func LoginStatus() (loggedIn bool, account string) {
	store, err := newOwnStore()
	if err != nil {
		return false, ""
	}
	return store.loggedIn(), redactAccount(store.data.AccountID)
}

// Logout removes cc-fleet's own token chain and stops the daemon (whose
// in-memory access token dies with it). ~/.codex is untouched.
func Logout() error {
	if err := withTokenLock(func() error {
		p, err := storePath()
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	return StopDaemon()
}

// redactAccount masks an account id for display (it is an identifier, not a secret,
// but full-value display is avoided in logs/UI for consistency).
func redactAccount(id string) string {
	if len(id) <= 6 {
		return id
	}
	return "…" + id[len(id)-6:]
}
