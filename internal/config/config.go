// Package config loads and stores the billkit CLI configuration in
// ~/.billkit/config.json, organised into named profiles (typically "test"
// and "live"), each holding an API key and optional API host override.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// DefaultBaseURL is the production BillKit API host.
const DefaultBaseURL = "https://api.billkit.eu"

// ErrNoProfile means the config file holds nothing matching the profile that
// was asked for. It is a sentinel so the command layer can recognise it and
// add the rest of the ways a key can be supplied, which this package does not
// know about.
var ErrNoProfile = errors.New("no BillKit profile configured — run `billkit login`")

// fileMode/dirMode are the only modes the config is ever allowed to have: it
// stores live secret keys, so nothing but the owner may read it.
//
// They are POSIX modes and only POSIX honours them. On Windows a Go FileMode
// reduces to one FILE_ATTRIBUTE_READONLY bit, which 0600 does not even set,
// and the file takes the DACL it inherits from its folder. The config is
// private there because %USERPROFILE% is private, not because of these
// constants, which is why warnIfLoosePerms asks a different question on
// Windows and why the docs say so rather than promising 0600 everywhere.
const (
	fileMode os.FileMode = 0o600
	dirMode  os.FileMode = 0o700
)

// warnWriter receives permission warnings. It is a variable so tests can
// capture what a user would see on stderr.
var warnWriter io.Writer = os.Stderr

// warnOnce keeps the loose-permission notice to one line per process, since
// Load() runs several times in a single invocation.
var warnOnce sync.Once

// Profile is a single named set of credentials.
type Profile struct {
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url,omitempty"`
}

// Config is the on-disk shape of ~/.billkit/config.json.
type Config struct {
	DefaultProfile string             `json:"default_profile"`
	Profiles       map[string]Profile `json:"profiles"`
}

// Mode returns "test" or "live" inferred from a bk_test_/bk_live_ key prefix.
func Mode(apiKey string) string {
	switch {
	case strings.HasPrefix(apiKey, "bk_live_"):
		return "live"
	case strings.HasPrefix(apiKey, "bk_test_"):
		return "test"
	default:
		return ""
	}
}

// Path returns the config file location, honoring BILLKIT_CONFIG_HOME.
func Path() (string, error) {
	if custom := os.Getenv("BILLKIT_CONFIG_HOME"); custom != "" {
		return filepath.Join(custom, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".billkit", "config.json"), nil
}

// Load reads the config, returning an empty (non-nil) config if none exists.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Profiles: map[string]Profile{}}, nil
	}
	if err != nil {
		return nil, err
	}
	warnIfLoosePerms(path)
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	return cfg, nil
}

// Save writes the config atomically with 0600 perms (it holds secret keys).
//
// The write goes to a fresh temp file in the same directory and is then
// renamed over the target. That buys three things a plain os.WriteFile does
// not: a brand-new inode always ends up 0600 even when a config.json already
// existed at 0644, an interrupted login cannot truncate the other profile's
// key, and a symlink planted at config.json is replaced rather than written
// through.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	// MkdirAll is a no-op when the directory already exists, so it never
	// tightens a pre-existing 0755 ~/.billkit. Chmod does.
	if err := os.Chmod(dir, dirMode); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeSecretFile(path, data)
}

// writeSecretFile writes data to path atomically, owner-only.
func writeSecretFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	// CreateTemp opens with O_CREATE|O_EXCL and mode 0600, and retries with a
	// fresh random name on collision, so a leftover temp file from a killed
	// run can never wedge the write the way a fixed ".tmp" name would.
	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	// CreateTemp honors the process umask on some platforms, so be explicit.
	if err = tmp.Chmod(fileMode); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		// The deferred cleanup keys off err, and tmp is already closed; a
		// second Close there is harmless.
		return err
	}
	return nil
}

// warnIfLoosePerms tells the user, once, when the file holding their secret
// keys may be readable by anyone else on the machine. It warns rather than
// refuses: a hard failure would strand anyone whose config arrived from a
// dotfiles checkout or a CI cache, and the next Save() re-tightens it anyway.
func warnIfLoosePerms(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		home = ""
	}
	msg := loosePermWarning(runtime.GOOS, path, info.Mode().Perm(), home)
	if msg == "" {
		return
	}
	warnOnce.Do(func() {
		fmt.Fprint(warnWriter, msg)
	})
}

// loosePermWarning returns what the user should be told about the protection
// on their config file, or "" when there is nothing to say. goos and home are
// parameters rather than package lookups so the Windows branch is testable
// from any host.
//
// The two platforms need different answers because a Go FileMode means
// different things on each.
//
// On POSIX the mode is the access control, so the group/other bits answer the
// question directly.
//
// On Windows it answers nothing. os.Stat synthesises 0666 for any writable
// file and 0444 for a read-only one, out of a single FILE_ATTRIBUTE_READONLY
// bit; the real access control is the DACL, which those bits never reflect.
// Reading them as POSIX bits means every run on Windows prints "mode 0666,
// other users can read your keys" over a perfectly private file in the user's
// own profile, and tells the user to run chmod, which Windows does not have.
// A warning that fires every time and cannot be acted on is one people learn
// to scroll past, which costs us the one case that matters. So Windows gets
// the question it can actually answer: is this file inside the user profile,
// whose DACL grants only the owner and administrators, or has
// BILLKIT_CONFIG_HOME put it somewhere like C:\ProgramData where the inherited
// DACL may let every account on the machine read it.
func loosePermWarning(goos, path string, perm os.FileMode, home string) string {
	if goos == "windows" {
		if home == "" || pathUnder(home, path, true) {
			return ""
		}
		return fmt.Sprintf(
			"! %s is outside your user profile, so its protection is whatever the\n"+
				"  containing folder's permissions allow, which may include every account on\n"+
				"  this machine. Keep the config under %s, or restrict the folder to your\n"+
				"  own account.\n", path, home)
	}
	if perm&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf(
		"! %s is mode %04o: other users on this machine can read your BillKit API keys.\n"+
			"  Fix it with: chmod 600 %s\n", path, perm, path)
}

// pathUnder reports whether path lies inside dir. fold compares
// case-insensitively, which is what Windows paths need.
func pathUnder(dir, path string, fold bool) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	if fold {
		return strings.HasPrefix(strings.ToLower(path), strings.ToLower(dir))
	}
	return strings.HasPrefix(path, dir)
}

// ValidateBaseURL rejects an API host that is not a plain http(s) URL. The
// stored base_url is followed with an `Authorization: Bearer bk_live_…`
// header attached, so anything that is not an ordinary origin (a file://
// path, a URL carrying embedded credentials, a bare hostname with no scheme)
// is refused rather than dialled.
func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid API host %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid API host %q: expected an http:// or https:// URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid API host %q: no host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("invalid API host %q: URLs with embedded credentials are not accepted", raw)
	}
	return nil
}

// CheckTransport decides whether a key of this kind may be sent to this host.
//
// A secret key is scoped to a host, so it must never travel over a channel
// that can be read or redirected. https is always fine. Plain http is only
// ever legitimate for the local-mock and integration-test case, which means
// loopback plus a test key; anything else is refused rather than dialled,
// because there is no configuration in which a live key belongs on the wire
// in the clear.
func CheckTransport(baseURL, apiKey string) error {
	if err := ValidateBaseURL(baseURL); err != nil {
		return err
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "https" {
		return err
	}
	if Mode(apiKey) == "live" {
		return fmt.Errorf("refusing to send a live API key to %s over plain HTTP: a live key requires an https:// host", baseURL)
	}
	if !isLoopback(u) {
		return fmt.Errorf("refusing to send an API key to %s over plain HTTP: use https://, or http:// only for a loopback host", baseURL)
	}
	if Mode(apiKey) != "test" {
		return fmt.Errorf("refusing to send this API key to %s over plain HTTP: http:// is only accepted for a loopback host with a bk_test_ key", baseURL)
	}
	return nil
}

// isLoopback reports whether the URL names this machine.
func isLoopback(u *url.URL) bool {
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Set stores a profile and marks it the default when it's the first one.
func (c *Config) Set(name string, p Profile) {
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.Profiles[name] = p
	if c.DefaultProfile == "" {
		c.DefaultProfile = name
	}
}

// Delete removes a profile, clearing the default when the deleted profile
// was the default. It reports whether a profile was actually removed.
//
// It deliberately does NOT promote a survivor. It used to pick the
// lowest-named one, which sorts "live" ahead of "test": logging out of
// test silently made live the default, so the command whose whole purpose
// is reducing exposure quietly pointed the next command at real money.
// Choosing a profile is now always an explicit act (`billkit config use`),
// and the caller is expected to say what the resulting state is.
func (c *Config) Delete(name string) bool {
	if _, ok := c.Profiles[name]; !ok {
		return false
	}
	delete(c.Profiles, name)
	if c.DefaultProfile == name {
		c.DefaultProfile = ""
	}
	return true
}

// Resolve returns the profile to use: the named one, else the default, else
// the sole profile. The returned profile always has a non-empty BaseURL.
func (c *Config) Resolve(name string) (Profile, string, error) {
	if name == "" {
		name = c.DefaultProfile
	}
	if name == "" && len(c.Profiles) == 1 {
		for only := range c.Profiles {
			name = only
		}
	}
	p, ok := c.Profiles[name]
	if !ok {
		return Profile{}, "", ErrNoProfile
	}
	// An absent base_url means "the production host". Anything actually
	// stored has to hold up, and the error names the file so the user can go
	// and look at what is in it.
	if p.BaseURL == "" {
		p.BaseURL = DefaultBaseURL
	} else if err := ValidateBaseURL(p.BaseURL); err != nil {
		path, pathErr := Path()
		if pathErr != nil {
			path = "the config file"
		}
		return Profile{}, "", fmt.Errorf("profile %q in %s: %w", name, path, err)
	}
	return p, name, nil
}

// SetDefault makes an existing profile the one later commands use. It reports
// whether the profile existed.
func (c *Config) SetDefault(name string) bool {
	if _, ok := c.Profiles[name]; !ok {
		return false
	}
	c.DefaultProfile = name
	return true
}
