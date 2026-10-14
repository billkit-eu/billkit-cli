// Package config loads and stores the billkit CLI configuration, organised
// into named profiles (typically "test" and "live"), each holding an API key
// (or a reference to one in the OS keychain) and an optional API host
// override.
//
// The file lives at the platform's conventional place (see Dir), and a config
// left at the pre-1.0 location, ~/.billkit/config.json, is moved there on the
// first Load (see migrate.go).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
)

// DefaultBaseURL is the production BillKit API host.
const DefaultBaseURL = "https://api.billkit.eu"

// SchemaVersion is the version of the on-disk shape this build writes.
//
// A file with no "version" key predates versioning (every 0.x release) and
// reads as version 0, which is the same shape minus the keychain reference.
// A file whose version is higher than this one was written by a newer
// billkit: it is read on a best-effort basis, so a downgrade still works,
// but never rewritten, because a rewrite could drop or misread a field this
// build does not understand. Unknown keys are preserved on every rewrite for
// the same reason, at every level.
const SchemaVersion = 1

// ErrNoProfile means no stored profile could be selected: nothing is
// configured, or the configured default names nothing. It is a sentinel so the
// command layer can recognise it and add the rest of the ways a key can be
// supplied, which this package does not know about.
var ErrNoProfile = errors.New("no BillKit profile configured; run `billkit login`")

// ErrUnknownProfile means a profile was asked for by name and no profile of
// that name exists. Unlike ErrNoProfile it is the caller's argument that is
// wrong, so the command layer reports it as a bad invocation.
var ErrUnknownProfile = errors.New("no such profile")

// ErrNewerSchema refuses to rewrite a config a newer billkit wrote.
var ErrNewerSchema = errors.New("this config was written by a newer billkit")

// fileMode/dirMode are the only modes the config is ever allowed to have: it
// stores live secret keys, so nothing but the owner may read it.
//
// They are POSIX modes and only POSIX honours them. On Windows a Go FileMode
// reduces to one FILE_ATTRIBUTE_READONLY bit, which 0600 does not even set,
// and the file takes the DACL it inherits from its folder. The config is
// private there because %APPDATA% is private, not because of these
// constants, which is why warnIfLoosePerms asks a different question on
// Windows and why the docs say so rather than promising 0600 everywhere.
const (
	fileMode os.FileMode = 0o600
	dirMode  os.FileMode = 0o700
)

// warnWriter receives permission and migration notices. It is a variable so
// tests can capture what a user would see on stderr.
var warnWriter io.Writer = os.Stderr

// warnMu serialises writes to warnWriter: the config lock admits one writer
// per config, but notices can come from several goroutines of one process.
var warnMu sync.Mutex

// warnf writes one notice to warnWriter.
func warnf(format string, args ...any) {
	warnMu.Lock()
	defer warnMu.Unlock()
	fmt.Fprintf(warnWriter, format, args...)
}

// warnOnce keeps the loose-permission notice to one line per process, since
// Load() runs several times in a single invocation.
var warnOnce sync.Once

// Profile is a single named set of credentials.
//
// Exactly one of APIKey and Keychain is set on a stored profile. Keychain is
// the account under which the OS keychain holds the key; only live keys are
// ever put there (see StoreKey), so a keychain profile is a live profile.
type Profile struct {
	APIKey   string
	BaseURL  string
	Keychain string

	// extra holds the keys this build does not know, so a rewrite gives
	// back every field a newer billkit put here.
	extra map[string]json.RawMessage
}

// Mode reports "live" or "test" for the profile's key, or "" when unknown.
// A keychain profile is live by construction, and saying so here means the
// key does not have to be read out of the keychain just to label it.
func (p Profile) Mode() string {
	if p.Keychain != "" {
		return "live"
	}
	return Mode(p.APIKey)
}

// Storage names where the key lives: "keychain", "file", or "" for none.
func (p Profile) Storage() string {
	switch {
	case p.Keychain != "":
		return "keychain"
	case p.APIKey != "":
		return "file"
	default:
		return ""
	}
}

var profileKnownKeys = []string{"api_key", "base_url", "keychain_account"}

// MarshalJSON writes the known fields plus every preserved unknown one.
func (p Profile) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	for k, v := range p.extra {
		out[k] = v
	}
	if p.APIKey != "" {
		out["api_key"] = p.APIKey
	}
	if p.BaseURL != "" {
		out["base_url"] = p.BaseURL
	}
	if p.Keychain != "" {
		out["keychain_account"] = p.Keychain
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads the known fields and keeps the rest verbatim.
func (p *Profile) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = Profile{}
	for key, dst := range map[string]*string{"api_key": &p.APIKey, "base_url": &p.BaseURL, "keychain_account": &p.Keychain} {
		if v, ok := raw[key]; ok {
			if err := json.Unmarshal(v, dst); err != nil {
				return fmt.Errorf("profile field %q: %w", key, err)
			}
		}
	}
	p.extra = without(raw, profileKnownKeys)
	return nil
}

// Config is the on-disk shape of config.json.
type Config struct {
	Version        int
	DefaultProfile string
	Profiles       map[string]Profile

	extra map[string]json.RawMessage
	// orphans are keychain accounts no profile refers to any more. They are
	// deleted by Save, after the file that stopped referring to them is on
	// disk, so a failed save never leaves a profile pointing at nothing.
	orphans []string
	// created are keychain accounts StoreKey made in this process that no
	// saved file refers to yet. A successful Save deletes any of them the
	// file it wrote does not reference; a failed one deletes them all,
	// because nothing on disk can reach them.
	created []string

	// path is the file this config was read from, and legacy marks it as the
	// pre-1.0 ~/.billkit/config.json read in place because the move to the
	// new location failed. Save never writes a legacy config to the new
	// location without finishing the move (see saveLegacy).
	path   string
	legacy bool
	// locked is set while Update holds the config lock for this config, so
	// a Save inside the callback does not try to take it a second time.
	locked bool
}

var configKnownKeys = []string{"version", "default_profile", "profiles"}

// MarshalJSON writes the known fields plus every preserved unknown one.
func (c Config) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	for k, v := range c.extra {
		out[k] = v
	}
	out["version"] = c.Version
	out["default_profile"] = c.DefaultProfile
	profiles := c.Profiles
	if profiles == nil {
		profiles = map[string]Profile{}
	}
	out["profiles"] = profiles
	return json.Marshal(out)
}

// UnmarshalJSON reads the known fields and keeps the rest verbatim.
func (c *Config) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = Config{}
	if v, ok := raw["version"]; ok {
		if err := json.Unmarshal(v, &c.Version); err != nil {
			return fmt.Errorf("config field \"version\": %w", err)
		}
	}
	if v, ok := raw["default_profile"]; ok {
		if err := json.Unmarshal(v, &c.DefaultProfile); err != nil {
			return fmt.Errorf("config field \"default_profile\": %w", err)
		}
	}
	if v, ok := raw["profiles"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &c.Profiles); err != nil {
			return fmt.Errorf("config field \"profiles\": %w", err)
		}
	}
	c.extra = without(raw, configKnownKeys)
	return nil
}

// without returns raw minus the named keys, or nil when nothing is left.
func without(raw map[string]json.RawMessage, known []string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range raw {
		if !slices.Contains(known, k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// configHomeEnv relocates the whole config directory. Its presence is also
// what tells Save() the directory is the user's rather than the CLI's; see
// ensureConfigDir.
const configHomeEnv = "BILLKIT_CONFIG_HOME"

// Dir returns the directory holding config.json.
//
// BILLKIT_CONFIG_HOME wins when set. Otherwise it is the platform's own
// answer, the same one the Stripe CLI and gh give: $XDG_CONFIG_HOME/billkit,
// defaulting to ~/.config/billkit, on Linux and macOS, and
// %APPDATA%\billkit on Windows. macOS deliberately does not get
// ~/Library/Application Support, which is what os.UserConfigDir answers
// there: a developer CLI's config is expected under ~/.config, and that is
// where dotfile managers look for it.
func Dir() (string, error) {
	if custom := os.Getenv(configHomeEnv); custom != "" {
		return custom, nil
	}
	return defaultDir(runtime.GOOS, os.Getenv, os.UserHomeDir)
}

// defaultDir is Dir without the override, with the platform and environment
// as parameters so every branch is testable from any host.
func defaultDir(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	if goos == "windows" {
		if appData := getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "billkit"), nil
		}
		h, err := home()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, "AppData", "Roaming", "billkit"), nil
	}
	// The XDG spec says a relative value is invalid and must be ignored.
	if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "billkit"), nil
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".config", "billkit"), nil
}

// Path returns the config file location.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config, returning an empty (non-nil) config if none exists.
// A config still at the pre-1.0 location is moved first, under the config
// lock; see migrate.go. A caller that means to change the config and save it
// should use Update instead, which holds the lock from the read to the write.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	if _, ok := legacyCandidate(path); !ok {
		return loadFrom(path)
	}
	lock, err := acquireLock()
	if err != nil {
		warnf("! Could not lock the BillKit config (%v), so a pre-1.0 config is read where it is.\n", err)
		return LoadReadOnly()
	}
	defer lock.release()
	return loadLocked(path)
}

// loadFrom reads and parses one config file.
func loadFrom(path string) (*Config, error) {
	// G304: the path is this package's own: Path(), or the pre-1.0 location
	// it migrates from. Reading the config file is what Load is for, and
	// there is no caller-supplied path to sanitise.
	data, err := os.ReadFile(path) //nolint:gosec
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Version: SchemaVersion, Profiles: map[string]Profile{}}, nil
	}
	if err != nil {
		return nil, err
	}
	warnIfLoosePerms(path)
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s is not a valid billkit config: %w", path, err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	cfg.path = path
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
//
// The containing directory is handled by ensureConfigDir, which is narrower
// than it looks, so read the note there before widening it again.
//
// Keychain entries no profile refers to any more are deleted afterwards, and
// only afterwards: if the write fails, the old file still points at them.
//
// Save takes the config lock for the write. That serialises it against every
// other writer but cannot make a Load earlier in the same command atomic with
// it; Update can, and is what the commands use.
func (c *Config) Save() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if c.locked {
		return c.saveLocked(path)
	}
	if err := c.checkWritable(); err != nil {
		c.rollback()
		return err
	}
	lock, err := acquireLock()
	if err != nil {
		c.rollback()
		return err
	}
	defer lock.release()
	return c.saveLocked(path)
}

// checkWritable refuses a config a newer billkit wrote. It runs before
// anything is put in the keychain, so a refused save leaves nothing behind.
func (c *Config) checkWritable() error {
	if c.Version > SchemaVersion {
		return fmt.Errorf("%w (schema version %d; this build understands %d), so it will not be rewritten: upgrade billkit",
			ErrNewerSchema, c.Version, SchemaVersion)
	}
	return nil
}

// saveLocked writes the config to path, or for a legacy config finishes the
// move or writes it back where it came from. The caller holds the lock.
func (c *Config) saveLocked(path string) error {
	if err := c.checkWritable(); err != nil {
		c.rollback()
		return err
	}
	var err error
	if c.legacy {
		err = c.saveLegacy(path)
	} else {
		err = c.writeTo(path)
	}
	if err != nil {
		c.rollback()
		return err
	}
	c.commit()
	return nil
}

// saveLegacy saves a config that is still the pre-1.0 file read in place.
//
// Writing it to the new location as it stands would put every live key in it
// there in plain text while the old copy stayed behind for good. So it either
// finishes the move properly (keychain, new file, old file deleted) or, when
// the new location still cannot be written, writes it back to the old one
// and says so.
func (c *Config) saveLegacy(path string) error {
	err := c.completeMigration(path)
	if err == nil {
		return nil
	}
	warnf("! Could not move your BillKit config from %s to %s: %v\n"+
		"  Saved it at its old location instead; the move is tried again on the next run.\n", c.path, path, err)
	return c.writeTo(c.path)
}

// writeTo writes the config file at path, stamped with SchemaVersion.
func (c *Config) writeTo(path string) error {
	if err := ensureConfigDir(filepath.Dir(path)); err != nil {
		return err
	}
	out := *c
	out.Version = SchemaVersion
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := writeSecretFile(path, append(data, '\n')); err != nil {
		return err
	}
	c.Version = SchemaVersion
	return nil
}

// ProfileNames returns the configured profile names, sorted.
func (c *Config) ProfileNames() []string {
	return slices.Sorted(maps.Keys(c.Profiles))
}

// ensureConfigDir makes sure the directory holding config.json exists and is
// as private as this CLI is entitled to make it.
//
// Save used to run os.Chmod(dir, 0700) unconditionally on every write. The
// intent was right, since MkdirAll only applies its mode to directories it
// actually creates, so it never tightens a config directory that already
// exists at 0755, but the reach was not: BILLKIT_CONFIG_HOME can point at a repo
// checkout, at $HOME itself, or at a shared CI workspace, and then `billkit
// login` silently re-moded a directory full of somebody else's files, for
// everything else in it.
//
// So the tightening is now scoped to a directory the CLI can claim:
//
//   - one it creates here is 0700 from birth, which is MkdirAll's mode and
//     which a umask can only narrow further;
//   - the default directory (Dir without BILLKIT_CONFIG_HOME, which always
//     ends in a directory named billkit) is still chmodded, because it is
//     ours and nothing else lives in it;
//   - a pre-existing directory the user named with BILLKIT_CONFIG_HOME is
//     left exactly as it was, with a warning when group or other can read it.
//
// Leaving it alone is safe because the secret is in the file, not in the
// directory, and writeSecretFile keeps that at 0600 either way. A loose
// directory lets other accounts *list* it; it does not let them read a key.
func ensureConfigDir(dir string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dir, dirMode)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if os.Getenv(configHomeEnv) == "" {
		return os.Chmod(dir, dirMode)
	}
	if msg := looseDirWarning(runtime.GOOS, dir, info.Mode().Perm()); msg != "" {
		warnf("%s", msg)
	}
	return nil
}

// looseDirWarning is the directory counterpart of loosePermWarning, for the
// one case the CLI will not fix itself. Windows is excluded for the same
// reason as there: a Go FileMode is one read-only attribute bit, so the
// group/other bits are synthesised and mean nothing.
func looseDirWarning(goos, dir string, perm os.FileMode) string {
	if goos == "windows" || perm&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf(
		"! %s is mode %04o: other users on this machine can list the directory holding\n"+
			"  your BillKit config. The config file itself stays 0600. %s points here, so\n"+
			"  the CLI leaves a directory it did not create alone.\n"+
			"  Tighten it yourself with: chmod 700 %s\n", dir, perm, configHomeEnv, dir)
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
	// The file's bytes are on disk; flush the directory too, so a crash now
	// cannot bring back the entry the rename replaced.
	syncDir(dir)
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
		warnf("%s", msg)
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
// was the default (it never reassigns it). It reports whether a profile was actually removed. A key
// the profile kept in the OS keychain is deleted from it by the next Save.
//
// It deliberately does NOT promote a survivor. It used to pick the
// lowest-named one, which sorts "live" ahead of "test": logging out of
// test silently made live the default, so the command whose whole purpose
// is reducing exposure quietly pointed the next command at real money.
// Choosing a profile is now always an explicit act (`billkit config use`),
// and the caller is expected to say what the resulting state is.
func (c *Config) Delete(name string) bool {
	p, ok := c.Profiles[name]
	if !ok {
		return false
	}
	if p.Keychain != "" {
		c.orphans = append(c.orphans, p.Keychain)
	}
	delete(c.Profiles, name)
	if c.DefaultProfile == name {
		c.DefaultProfile = ""
	}
	return true
}

// Resolve returns the profile to use: the named one, else the default, else
// the sole profile. The returned profile always has a non-empty BaseURL, and
// its APIKey filled in, from the OS keychain when that is where it lives.
//
// A name that was asked for and does not exist is ErrUnknownProfile, naming
// the profiles that do; nothing to select at all is ErrNoProfile.
func (c *Config) Resolve(name string) (Profile, string, error) {
	explicit := name != ""
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
		if explicit {
			return Profile{}, "", c.UnknownProfileError(name)
		}
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
	if p.Keychain != "" {
		key, err := keyringGet(p.Keychain)
		if err != nil {
			return Profile{}, "", fmt.Errorf("%w: profile %q keeps its API key in the OS keychain, and reading it failed (%v). "+
				"Run `billkit login --profile %s` to store it again", ErrKeychainRead, name, err, name)
		}
		p.APIKey = key
	}
	return p, name, nil
}

// UnknownProfileError is the error for a profile name that is not
// configured. It wraps ErrUnknownProfile and lists what is.
func (c *Config) UnknownProfileError(name string) error {
	known := c.ProfileNames()
	if len(known) == 0 {
		return fmt.Errorf("%w %q: no profiles are configured; run `billkit login`", ErrUnknownProfile, name)
	}
	return fmt.Errorf("%w %q; configured profiles: %s", ErrUnknownProfile, name, strings.Join(known, ", "))
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
