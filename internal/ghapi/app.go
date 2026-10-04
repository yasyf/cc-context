package ghapi

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
)

const (
	// AppWatchMargin is the life a token must have left before a long-lived
	// reader such as gh run watch takes it, since the reader cannot swap it.
	AppWatchMargin = 50 * time.Minute
	// AppRefreshMargin is the life left at which a cached token is replaced.
	AppRefreshMargin = 5 * time.Minute

	installedTTL    = 24 * time.Hour
	notInstalledTTL = time.Hour
	appJWTLifetime  = 9 * time.Minute
	appJWTBackdate  = time.Minute
	appDownFor      = 5 * time.Minute
)

var writeOnlyPermissions = map[string]bool{"workflows": true}

var (
	errNotInstalled = errors.New("github app is not installed on the repository")
	errAppDown      = errors.New("github app failed recently")
)

type appConfig struct {
	Name              string   `toml:"name"`
	ClientID          string   `toml:"client_id"`
	PrivateKeyCommand []string `toml:"private_key_command"`
}

// AppConfigPath is the file that names the GitHub App ccx reads through:
// $XDG_CONFIG_HOME/ccx/github-app.toml, else ~/.config/ccx/github-app.toml.
func AppConfigPath(ctx context.Context) (string, error) {
	if dir := render.Getenv(ctx, "XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "ccx", "github-app.toml"), nil
	}
	home, err := render.Home(ctx)
	if err != nil {
		return "", fmt.Errorf("ghapi: resolve home: %w", err)
	}
	return filepath.Join(home, ".config", "ccx", "github-app.toml"), nil
}

func loadAppConfig(ctx context.Context) (*appConfig, error) {
	path, err := AppConfigPath(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the user's own ccx config file
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ghapi: read %s: %w", path, err)
	}
	var cfg appConfig
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("ghapi: parse %s: %w", path, err)
	}
	if cfg.ClientID == "" || len(cfg.PrivateKeyCommand) == 0 {
		return nil, fmt.Errorf("ghapi: %s needs client_id and private_key_command", path)
	}
	if cfg.Name == "" {
		cfg.Name = cfg.ClientID
	}
	return &cfg, nil
}

type appLoader struct {
	once sync.Once
	src  *appSource
	err  error
	load func(context.Context) (*appSource, error)
}

func (l *appLoader) get(ctx context.Context) (*appSource, error) {
	l.once.Do(func() { l.src, l.err = l.load(ctx) })
	return l.src, l.err
}

func defaultAppLoader(base string) *appLoader {
	return &appLoader{load: func(ctx context.Context) (*appSource, error) {
		cfg, err := loadAppConfig(ctx)
		if err != nil || cfg == nil {
			return nil, err
		}
		root, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("ghapi: resolve user cache dir: %w", err)
		}
		return newAppSource(*cfg, base, filepath.Join(root, "cc-context", "github-app", cfg.ClientID)), nil
	}}
}

type appSource struct {
	cfg  appConfig
	base string
	dir  string
	http *http.Client
	now  func() time.Time
	key  func(context.Context) ([]byte, error)
	warn sync.Once
}

func newAppSource(cfg appConfig, base, dir string) *appSource {
	a := &appSource{cfg: cfg, base: strings.TrimSuffix(base, "/"), dir: dir, http: &http.Client{}, now: time.Now}
	a.key = a.runKeyCommand
	return a
}

type installation struct {
	ID        int64     `json:"id"`
	CheckedAt time.Time `json:"checked_at"`
}

type appOutage struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

type appToken struct {
	Token       string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

func (a *appSource) token(ctx context.Context, repo string, margin time.Duration, stale string) (appToken, int64, error) {
	if id, ok := a.cachedInstallation(repo); ok {
		if id == 0 {
			return appToken{}, 0, errNotInstalled
		}
		if tok, ok := a.cachedToken(id, margin); ok && tok.Token != stale {
			return tok, id, nil
		}
	}
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return appToken{}, 0, fmt.Errorf("ghapi: create %s: %w", a.dir, err)
	}
	var tok appToken
	var id int64
	err := cache.WithLock(ctx, a.dir, "mint", func() error {
		if err := a.outage(); err != nil {
			return err
		}
		bearer := sync.OnceValues(func() (string, error) { return a.jwt(ctx) })
		known := false
		id, known = a.cachedInstallation(repo)
		if !known {
			jwt, err := bearer()
			if err != nil {
				return err
			}
			if id, err = a.lookupInstallation(ctx, jwt, repo); err != nil {
				return err
			}
			if err := a.storeInstallation(repo, id); err != nil {
				return err
			}
		}
		if id == 0 {
			return errNotInstalled
		}
		if cached, ok := a.cachedToken(id, margin); ok && cached.Token != stale {
			tok = cached
			return nil
		}
		jwt, err := bearer()
		if err != nil {
			return err
		}
		if tok, err = a.mint(ctx, jwt, id); err != nil {
			return err
		}
		return a.storeToken(id, tok)
	})
	return tok, id, err
}

func (a *appSource) unavailable(ctx context.Context, err error) {
	if errors.Is(err, errNotInstalled) {
		return
	}
	if !errors.Is(err, errAppDown) && ctx.Err() == nil {
		a.storeOutage(err)
	}
	a.warn.Do(func() {
		slog.Warn("ghapi: github app unavailable, reading as the gh user", "app", a.cfg.Name, "err", err)
	})
}

func (a *appSource) outagePath() string {
	return filepath.Join(a.dir, "outage.json")
}

func (a *appSource) outage() error {
	raw, err := os.ReadFile(a.outagePath())
	if err != nil {
		return nil
	}
	var down appOutage
	if json.Unmarshal(raw, &down) != nil || !a.now().Before(down.Until) {
		return nil
	}
	return fmt.Errorf("%w, reading as the gh user until %s: %s", errAppDown, down.Until.Local().Format(time.Kitchen), down.Reason)
}

func (a *appSource) storeOutage(cause error) {
	raw, err := json.Marshal(appOutage{Until: a.now().Add(appDownFor), Reason: cause.Error()})
	if err == nil {
		err = cache.Store(a.outagePath(), raw, 0o600)
	}
	if err != nil {
		slog.Warn("ghapi: record github app outage", "app", a.cfg.Name, "err", err)
	}
}

func (a *appSource) cachedInstallation(repo string) (int64, bool) {
	known, ok := a.readInstallations()[repo]
	if !ok {
		return 0, false
	}
	ttl := installedTTL
	if known.ID == 0 {
		ttl = notInstalledTTL
	}
	return known.ID, a.now().Sub(known.CheckedAt) < ttl
}

func (a *appSource) readInstallations() map[string]installation {
	known := map[string]installation{}
	raw, err := os.ReadFile(filepath.Join(a.dir, "installations.json"))
	if err != nil {
		return known
	}
	_ = json.Unmarshal(raw, &known)
	return known
}

func (a *appSource) storeInstallation(repo string, id int64) error {
	known := a.readInstallations()
	known[repo] = installation{ID: id, CheckedAt: a.now()}
	raw, err := json.Marshal(known)
	if err != nil {
		return fmt.Errorf("ghapi: encode installations: %w", err)
	}
	return cache.Store(filepath.Join(a.dir, "installations.json"), raw, 0o600)
}

func (a *appSource) tokenPath(id int64) string {
	return filepath.Join(a.dir, strconv.FormatInt(id, 10)+".json")
}

func (a *appSource) cachedToken(id int64, margin time.Duration) (appToken, bool) {
	raw, err := os.ReadFile(a.tokenPath(id))
	if err != nil {
		return appToken{}, false
	}
	var tok appToken
	if json.Unmarshal(raw, &tok) != nil || tok.Token == "" {
		return appToken{}, false
	}
	return tok, tok.ExpiresAt.Sub(a.now()) > margin
}

func (a *appSource) storeToken(id int64, tok appToken) error {
	raw, err := json.Marshal(tok)
	if err != nil {
		return fmt.Errorf("ghapi: encode installation token: %w", err)
	}
	return cache.Store(a.tokenPath(id), raw, 0o600)
}

func (a *appSource) lookupInstallation(ctx context.Context, jwt, repo string) (int64, error) {
	var found struct {
		ID int64 `json:"id"`
	}
	err := a.call(ctx, http.MethodGet, "/repos/"+repo+"/installation", jwt, nil, &found)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	return found.ID, err
}

func (a *appSource) mint(ctx context.Context, jwt string, id int64) (appToken, error) {
	var inst struct {
		Permissions map[string]string `json:"permissions"`
	}
	if err := a.call(ctx, http.MethodGet, fmt.Sprintf("/app/installations/%d", id), jwt, nil, &inst); err != nil {
		return appToken{}, err
	}
	read := map[string]string{}
	for name := range inst.Permissions {
		if !writeOnlyPermissions[name] {
			read[name] = "read"
		}
	}
	body, err := json.Marshal(map[string]any{"permissions": read})
	if err != nil {
		return appToken{}, fmt.Errorf("ghapi: encode token request: %w", err)
	}
	var tok appToken
	if err := a.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), jwt, body, &tok); err != nil {
		return appToken{}, err
	}
	return tok, nil
}

func (a *appSource) call(ctx context.Context, method, path, bearer string, body []byte, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	target := a.base + path
	req, err := http.NewRequestWithContext(reqCtx, method, target, reader)
	if err != nil {
		return fmt.Errorf("ghapi: build request %s %s: %w", method, target, err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("ghapi: %s %s: %w", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ghapi: read %s %s: %w", method, target, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(method, target, resp.StatusCode, resp.Header, payload, a.now())
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("ghapi: decode %s %s: %w", method, target, err)
	}
	return nil
}

func (a *appSource) jwt(ctx context.Context) (string, error) {
	raw, err := a.key(ctx)
	if err != nil {
		return "", err
	}
	key, err := parseRSAKey(raw)
	if err != nil {
		return "", err
	}
	now := a.now()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("ghapi: encode jwt header: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-appJWTBackdate).Unix(),
		"exp": now.Add(appJWTLifetime).Unix(),
		"iss": a.cfg.ClientID,
	})
	if err != nil {
		return "", fmt.Errorf("ghapi: encode jwt claims: %w", err)
	}
	signed := b64url(header) + "." + b64url(claims)
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("ghapi: sign app jwt: %w", err)
	}
	return signed + "." + b64url(sig), nil
}

func (a *appSource) runKeyCommand(ctx context.Context) ([]byte, error) {
	cmd := a.cfg.PrivateKeyCommand
	out, err := render.RunCLI(ctx, render.Ambient, cmd[0], cmd[1:])
	if err != nil {
		return nil, fmt.Errorf("ghapi: %s private_key_command: %w", a.cfg.Name, err)
	}
	return []byte(out), nil
}

func parseRSAKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(bytes.TrimSpace(raw))
	if block == nil {
		return nil, errors.New("ghapi: private_key_command printed no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ghapi: parse app private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ghapi: app private key is %T, not RSA", parsed)
	}
	return key, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

type appCredential struct {
	src  *appSource
	repo string
}

func (c *appCredential) get(ctx context.Context) (string, error) {
	tok, _, err := c.src.token(ctx, c.repo, AppRefreshMargin, "")
	return tok.Token, err
}

func (c *appCredential) refresh(ctx context.Context, stale string) error {
	_, _, err := c.src.token(ctx, c.repo, AppRefreshMargin, stale)
	return err
}
