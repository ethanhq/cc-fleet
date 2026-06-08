package codexproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

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
