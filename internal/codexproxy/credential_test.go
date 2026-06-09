package codexproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// setLogin skips the token write (and returns the context error) when its context is
// already cancelled — so a cancelled/abandoned login never persists an orphan token.
func TestSetLogin_SkipsOnCancelledContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, err := newOwnStore("")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.setLogin(ctx, &tokens{RefreshToken: "rt", AccountID: "acc"}); err == nil {
		t.Fatal("setLogin must return the context error when cancelled")
	}
	p, err := storePath("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("setLogin must not write the token file when cancelled, stat err = %v", err)
	}
}

// The default credential (empty ref or the legacy sentinel) maps to the unsuffixed
// legacy filenames; a named credential gets a "-<ref>" suffix on both the token file
// and its flock, and the two never name-diverge.
func TestCredentialFilenameMapping(t *testing.T) {
	for _, ref := range []string{"", SecretRef} {
		if got := tokenStoreFileFor(ref); got != "codex_oauth.json" {
			t.Fatalf("tokenStoreFileFor(%q) = %q, want legacy codex_oauth.json", ref, got)
		}
		if got := tokenLockFileFor(ref); got != ".cc-fleet-codex-token.lock" {
			t.Fatalf("tokenLockFileFor(%q) = %q, want legacy lock", ref, got)
		}
		if !IsDefaultCredentialRef(ref) {
			t.Fatalf("IsDefaultCredentialRef(%q) = false, want true", ref)
		}
	}
	if got := tokenStoreFileFor("work"); got != "codex_oauth-work.json" {
		t.Fatalf("named token file = %q", got)
	}
	if got := tokenLockFileFor("work"); got != ".cc-fleet-codex-token-work.lock" {
		t.Fatalf("named token lock = %q", got)
	}
	if IsDefaultCredentialRef("work") {
		t.Fatal("a named ref must not classify as the default credential")
	}
}

// A non-default ref that is not a path-safe identifier is rejected before it can
// become a token-file / flock name (so a CLI --credential can't escape the dir).
func TestValidateRef(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, bad := range []string{"../escape", "a/b", "x\x00y", "a b"} {
		if _, err := newOwnStore(bad); err == nil {
			t.Fatalf("newOwnStore(%q) should reject a path-unsafe ref", bad)
		}
		if err := withTokenLock(bad, func() error { return nil }); err == nil {
			t.Fatalf("withTokenLock(%q) should reject a path-unsafe ref", bad)
		}
	}
	// The default forms and a plain identifier are accepted.
	for _, ok := range []string{"", SecretRef, "codex-work"} {
		if _, err := newOwnStore(ok); err != nil {
			t.Fatalf("newOwnStore(%q) should accept, got %v", ok, err)
		}
	}
}

// sameCredential treats every default form as one credential (so a daemon recorded
// before multi-credential, with no Credential field, still matches the default), and
// keeps distinct names distinct.
func TestSameCredential(t *testing.T) {
	if !sameCredential("", SecretRef) {
		t.Fatal("empty and the sentinel are the same default credential")
	}
	if !sameCredential("work", "work") {
		t.Fatal("equal named refs match")
	}
	if sameCredential("", "work") || sameCredential(SecretRef, "work") {
		t.Fatal("the default credential must not match a named one")
	}
	if sameCredential("work", "other") {
		t.Fatal("two different named refs must not match")
	}
}

// writeStore drops a minimal own-store file for ref so LoginStatus/Logout can act on it.
func writeStore(t *testing.T, ref, account string) string {
	t.Helper()
	p, err := storePath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"refresh_token":"rt","account_id":"`+account+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Two credentials persist to independent files; a login under one is invisible to the
// other, and Logout(ref) removes only that ref's file.
func TestPerCredentialIsolationAndLogout(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.codex
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	defPath := writeStore(t, SecretRef, "acc-default")
	workPath := writeStore(t, "work", "acc-work")
	if defPath == workPath {
		t.Fatal("default and named credentials must use different files")
	}

	if ok, _ := LoginStatus(SecretRef); !ok {
		t.Fatal("default credential should read as logged in")
	}
	if ok, _ := LoginStatus("work"); !ok {
		t.Fatal("named credential should read as logged in")
	}

	if err := Logout("work"); err != nil {
		t.Fatalf("logout work: %v", err)
	}
	if _, err := os.Stat(workPath); !os.IsNotExist(err) {
		t.Fatal("logout(work) must remove the work token file")
	}
	if ok, _ := LoginStatus(SecretRef); !ok {
		t.Fatal("logout(work) must leave the default credential intact")
	}
}

// LogoutIfUnreferenced keeps a credential still claimed by a codex provider (the
// delete↔re-add race guard) and removes one no longer referenced.
func TestLogoutIfUnreferenced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &config.Config{Version: config.SchemaVersion, Vendors: map[string]*config.Vendor{
		"codex": {
			Name: "codex", BaseURL: "http://127.0.0.1:17222/",
			ModelsEndpoint: "http://127.0.0.1:17222/v1/models", DefaultModel: "gpt-5.5",
			SecretBackend: config.CodexOAuthBackend, SecretRef: config.CodexOAuthBackend,
			Protocol: config.ProtocolCodexOAuth, Enabled: true,
		},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	tok := writeStore(t, SecretRef, "acc")

	if err := LogoutIfUnreferenced(SecretRef); err != nil {
		t.Fatalf("referenced: %v", err)
	}
	if _, err := os.Stat(tok); err != nil {
		t.Fatalf("a still-referenced credential must be kept: %v", err)
	}

	delete(cfg.Vendors, "codex")
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if err := LogoutIfUnreferenced(SecretRef); err != nil {
		t.Fatalf("unreferenced: %v", err)
	}
	if _, err := os.Stat(tok); !os.IsNotExist(err) {
		t.Fatalf("an unreferenced credential must be removed, stat err = %v", err)
	}
}

// CLI-ride is offered only to the default credential; a named credential with no own
// login reports no active source even when ~/.codex is present.
func TestStatusReport_CLIRideDefaultOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCLIAuth(t, home, fakeJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())}))

	if st := StatusReport(SecretRef); !st.CLIRide || st.Active != "cli-ride" {
		t.Fatalf("default credential should ride ~/.codex: %+v", st)
	}
	if st := StatusReport("work"); st.CLIRide || st.Active != "none" {
		t.Fatalf("named credential must not ride ~/.codex: %+v", st)
	}
}

// Purge clears both the legacy and the per-credential token files + locks; under
// keepToken the token files survive (a login credential) while the locks still go.
func TestPurge_LegacyAndNamedTokenFiles(t *testing.T) {
	run := func(t *testing.T, keepToken bool) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		defTok := writeStore(t, SecretRef, "acc")
		workTok := writeStore(t, "work", "acc")
		defLock, _ := joinConfig(tokenLockFileFor(SecretRef))
		workLock, _ := joinConfig(tokenLockFileFor("work"))
		for _, p := range []string{defLock, workLock} {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		Purge(keepToken)

		for _, lock := range []string{defLock, workLock} {
			if _, err := os.Stat(lock); !os.IsNotExist(err) {
				t.Fatalf("token lock %s must always be purged", filepath.Base(lock))
			}
		}
		for _, tok := range []string{defTok, workTok} {
			_, err := os.Stat(tok)
			if keepToken && os.IsNotExist(err) {
				t.Fatalf("keepToken must retain %s", filepath.Base(tok))
			}
			if !keepToken && !os.IsNotExist(err) {
				t.Fatalf("purge must remove %s", filepath.Base(tok))
			}
		}
	}
	t.Run("remove", func(t *testing.T) { run(t, false) })
	t.Run("keep", func(t *testing.T) { run(t, true) })
}
