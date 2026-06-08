package codexproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// tokenStoreFile is cc-fleet's own credential store, independent of ~/.codex.
const tokenStoreFile = "codex_oauth.json"

// storeData is the on-disk shape (0600). The access token is NOT persisted — only
// the durable refresh chain + account id.
type storeData struct {
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	IDToken      string `json:"id_token,omitempty"`
}

// bearer is a resolved upstream credential plus the generation it came from, so a
// caller that sees a 401 can invalidate exactly that generation and force a refresh.
type bearer struct {
	accessToken string
	accountID   string
	generation  uint64
}

// tokenSource hands out a valid upstream bearer and refreshes as needed.
type tokenSource interface {
	// token returns a valid bearer, refreshing under its own lock if near expiry.
	token(ctx context.Context) (bearer, error)
	// invalidate forces the next token() to refresh if gen is still current.
	invalidate(gen uint64)
}

// ownStore is the production source: an independent refresh chain persisted to
// ~/.config/cc-fleet/codex_oauth.json. The cross-process token-store flock is
// held only for read/refresh/persist — never across the Responses stream — so a
// login process and the daemon's refresher cannot clobber each other's rotation
// (the in-process mutex alone cannot serialize two processes).
type ownStore struct {
	path  string
	oauth *oauthClient

	mu       sync.Mutex
	data     storeData
	access   string
	expiry   time.Time
	gen      uint64
	poisoned bool // a refresh succeeded but its rotated token failed to persist
}

func storePath() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tokenStoreFile), nil
}

// withTokenLock serializes token-store read/refresh/persist across processes.
// Standalone — held with none of the other flock scopes, so no ordering cycle.
func withTokenLock(fn func() error) error {
	p, err := joinConfig(".cc-fleet-codex-token.lock")
	if err != nil {
		return err
	}
	return config.WithFlock(p, fn)
}

func newOwnStore() (*ownStore, error) {
	p, err := storePath()
	if err != nil {
		return nil, err
	}
	// gen starts at 1 so an own-chain bearer is never generation 0, which the
	// cliRideStore reserves for its read-only CLI-ride token.
	s := &ownStore{path: p, oauth: newOAuthClient(), gen: 1}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *ownStore) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // not logged in yet; token() will return ErrReauth
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.data)
}

// save persists the durable chain atomically at 0600.
func (s *ownStore) save() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.AtomicWrite(s.path, b, 0o600)
}

// setLogin records a fresh chain from a completed device login and persists it
// under the token lock (a concurrently-refreshing daemon must not interleave).
func (s *ownStore) setLogin(tk *tokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := withTokenLock(func() error {
		s.data = storeData{RefreshToken: tk.RefreshToken, AccountID: tk.AccountID, IDToken: tk.IDToken}
		return s.save()
	}); err != nil {
		return err
	}
	s.access = tk.AccessToken
	if exp, ok := tokenExpiry(tk.AccessToken); ok {
		s.expiry = exp
	}
	s.poisoned = false
	s.gen++
	return nil
}

func (s *ownStore) token(ctx context.Context) (bearer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return bearer{}, ErrReauth
	}
	if s.access != "" && time.Now().Before(s.expiry.Add(-refreshSkew)) {
		return bearer{s.access, s.data.AccountID, s.gen}, nil
	}
	if err := withTokenLock(func() error { return s.refreshLocked(ctx) }); err != nil {
		return bearer{}, err
	}
	return bearer{s.access, s.data.AccountID, s.gen}, nil
}

// refreshLocked refreshes the access token. Runs under BOTH s.mu and the
// cross-process token lock; the lock covers read→refresh→persist only (the
// upstream Responses stream is never under it).
func (s *ownStore) refreshLocked(ctx context.Context) error {
	// Double-check from disk: another process (a login, a parallel refresher)
	// may have rotated the chain since this store last read it — refreshing
	// with the superseded token would trip OpenAI's reuse detection.
	if err := s.load(); err != nil {
		return err
	}
	if s.data.RefreshToken == "" {
		return ErrReauth
	}
	tk, err := s.oauth.refresh(ctx, s.data.RefreshToken)
	if err != nil {
		return err
	}
	// Persist-before-use: the old refresh token is already invalidated server-side,
	// so a persist failure means the only live chain is unsaved — poison and require
	// re-login rather than serve a token whose refresh token we can't recover.
	rotated := tk.RefreshToken != s.data.RefreshToken
	s.data.RefreshToken = tk.RefreshToken
	if tk.AccountID != "" {
		s.data.AccountID = tk.AccountID
	}
	if tk.IDToken != "" {
		s.data.IDToken = tk.IDToken
	}
	if rotated {
		if err := s.save(); err != nil {
			s.poisoned = true
			return ErrReauth
		}
	}
	s.access = tk.AccessToken
	if exp, ok := tokenExpiry(tk.AccessToken); ok {
		s.expiry = exp
	}
	s.gen++
	return nil
}

func (s *ownStore) invalidate(gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen == s.gen {
		s.expiry = time.Time{} // force a refresh on next token()
	}
}

func (s *ownStore) loggedIn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.RefreshToken != ""
}
