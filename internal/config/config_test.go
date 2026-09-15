package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// skipWithoutPOSIXModes bails out where file modes carry no meaning.
func skipWithoutPOSIXModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
}

// assertOwnerOnlyPerm checks that a path is readable by its owner and nobody
// else, and is a no-op where that cannot be expressed.
//
// Prefer this to skipping a whole test. A Go FileMode on Windows reduces to one
// FILE_ATTRIBUTE_READONLY bit, so a file written with 0600 stats back as 0666
// and the assertion is meaningless rather than failing honestly. Everything
// else the surrounding test covers (save, load, resolve, precedence) is
// platform-independent and is exactly the kind of thing that breaks on the
// platform nobody develops on, so it should keep running there.
func assertOwnerOnlyPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != want {
		t.Fatalf("%s perms = %04o, want %04o", path, perm, want)
	}
}

// captureWarnings redirects the permission warning and resets the once-guard,
// so each test sees its own output.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := warnWriter
	warnWriter = buf
	warnOnce = sync.Once{}
	t.Cleanup(func() {
		warnWriter = prev
		warnOnce = sync.Once{}
	})
	return buf
}

func TestModeFromKeyPrefix(t *testing.T) {
	cases := map[string]string{
		"sk_test_abc": "test",
		"sk_live_abc": "live",
		"garbage":     "",
		"":            "",
	}
	for key, want := range cases {
		if got := Mode(key); got != want {
			t.Errorf("Mode(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestSaveLoadResolveRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Profiles) != 0 {
		t.Fatalf("expected empty config, got %d profiles", len(cfg.Profiles))
	}

	cfg.Set("test", Profile{APIKey: "sk_test_x"})
	cfg.Set("live", Profile{APIKey: "sk_live_y", BaseURL: "https://self.example"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	// The file holds secrets → must be 0600.
	assertOwnerOnlyPerm(t, filepath.Join(dir, "config.json"), 0o600)

	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DefaultProfile != "test" {
		t.Fatalf("default profile = %q, want the first one set", loaded.DefaultProfile)
	}

	// Default resolution fills in the production base URL.
	p, name, err := loaded.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if name != "test" || p.APIKey != "sk_test_x" {
		t.Fatalf("resolve default = (%q, %q)", name, p.APIKey)
	}
	if p.BaseURL != DefaultBaseURL {
		t.Fatalf("resolve default base = %q, want %q", p.BaseURL, DefaultBaseURL)
	}

	// Named resolution keeps an explicit override.
	live, _, err := loaded.Resolve("live")
	if err != nil {
		t.Fatal(err)
	}
	if live.BaseURL != "https://self.example" {
		t.Fatalf("live base = %q", live.BaseURL)
	}
}

func TestResolveWithoutProfilesErrors(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	cfg, _ := Load()
	if _, _, err := cfg.Resolve(""); err == nil {
		t.Fatal("expected an error when no profiles are configured")
	}
}

func TestDelete(t *testing.T) {
	cfg := &Config{Profiles: map[string]Profile{}}
	cfg.Set("test", Profile{APIKey: "sk_test_x"})
	cfg.Set("live", Profile{APIKey: "sk_live_y"})
	if cfg.DefaultProfile != "test" {
		t.Fatalf("default = %q, want test (first Set)", cfg.DefaultProfile)
	}

	// Deleting the default clears it and promotes nothing. The old
	// behaviour reassigned to the lowest-named survivor, and "live" sorts
	// before "test": logging out of test silently made live the default,
	// so the next command spent real money. This assertion used to demand
	// exactly that, which is why the behaviour shipped.
	if !cfg.Delete("test") {
		t.Fatal("Delete(test) returned false")
	}
	if _, ok := cfg.Profiles["test"]; ok {
		t.Fatal("test profile still present")
	}
	if cfg.DefaultProfile != "" {
		t.Fatalf("default = %q, want empty: logging out must never promote a survivor, least of all live", cfg.DefaultProfile)
	}

	// Deleting a missing profile is a no-op false.
	if cfg.Delete("nope") {
		t.Fatal("Delete(nope) returned true")
	}

	// Deleting the last profile clears the default.
	if !cfg.Delete("live") {
		t.Fatal("Delete(live) returned false")
	}
	if cfg.DefaultProfile != "" {
		t.Fatalf("default = %q, want empty after deleting the last profile", cfg.DefaultProfile)
	}
}

// TestSaveTightensPreexistingFileAndDir is the P0-1 regression. os.WriteFile
// and os.MkdirAll only apply their mode when they create something, so a
// config.json that arrived from a dotfiles checkout, a CI cache, or a
// container image at 0644 kept 0644 and quietly took delivery of an sk_live_
// key. The old Save() passes every other test in this file, because they all
// start from an empty t.TempDir().
func TestSaveTightensPreexistingFileAndDir(t *testing.T) {
	skipWithoutPOSIXModes(t)

	dir := filepath.Join(t.TempDir(), "billkit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// MkdirAll is umask-dependent, so pin the mode we are testing against.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"default_profile":"","profiles":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set("live", Profile{APIKey: "sk_live_SECRET"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %04o, want 0600 — a live key is readable by every other account on this machine", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir perms = %04o, want 0700", perm)
	}

	// The atomic write must not leave its scratch file behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.json" {
			t.Errorf("leftover file in the config dir: %q", e.Name())
		}
	}
}

// TestSaveReplacesASymlinkRatherThanFollowingIt covers the other half of the
// same bug: os.WriteFile follows a symlink planted at config.json and writes
// the secret through it, at the target's mode.
func TestSaveReplacesASymlinkRatherThanFollowingIt(t *testing.T) {
	skipWithoutPOSIXModes(t)

	root := t.TempDir()
	dir := filepath.Join(root, "billkit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "planted.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)

	cfg := &Config{Profiles: map[string]Profile{"live": {APIKey: "sk_live_SECRET"}}, DefaultProfile: "live"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	planted, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(planted), "sk_live_SECRET") {
		t.Error("the key was written through the planted symlink into a 0644 file")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("config.json is still a symlink after Save()")
	}
}

func TestLoadWarnsWhenTheConfigIsWorldReadable(t *testing.T) {
	skipWithoutPOSIXModes(t)

	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"default_profile":"live","profiles":{"live":{"api_key":"sk_live_x"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	warnings := captureWarnings(t)

	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	got := warnings.String()
	if !strings.Contains(got, "chmod 600") || !strings.Contains(got, path) {
		t.Fatalf("expected a loose-permission warning naming the file, got %q", got)
	}

	// A correctly-moded file says nothing.
	quiet := captureWarnings(t)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	if quiet.String() != "" {
		t.Fatalf("unexpected warning for a 0600 config: %q", quiet.String())
	}
}

// TestLoosePermWarningOnWindowsIgnoresPOSIXModeBits is the P1-19 Windows
// regression. os.Stat on Windows synthesises 0666 for every writable file out
// of a single read-only attribute bit, so reading those bits as POSIX
// permissions made the CLI print "mode 0666: other users on this machine can
// read your BillKit API keys, fix it with: chmod 600" on every single run,
// over a file in the user's own profile, naming a command Windows does not
// have. The POSIX assertions in this file all skip on Windows, so nothing
// caught it.
func TestLoosePermWarningOnWindowsIgnoresPOSIXModeBits(t *testing.T) {
	home := filepath.Join("C:", "Users", "ada")
	inProfile := filepath.Join(home, ".billkit", "config.json")

	// 0666 is what a perfectly private Windows file reports.
	if got := loosePermWarning("windows", inProfile, 0o666, home); got != "" {
		t.Errorf("a config inside the user profile warned on Windows: %q", got)
	}
	// Case-insensitivity: Windows hands back either spelling.
	if got := loosePermWarning("windows", strings.ToUpper(inProfile), 0o666, home); got != "" {
		t.Errorf("a differently-cased path inside the user profile warned: %q", got)
	}
	// Outside the profile the folder's DACL is the only protection, and
	// BILLKIT_CONFIG_HOME=C:\ProgramData\billkit is the case worth naming.
	shared := filepath.Join("C:", "ProgramData", "billkit", "config.json")
	got := loosePermWarning("windows", shared, 0o666, home)
	if !strings.Contains(got, shared) || !strings.Contains(got, home) {
		t.Errorf("expected a warning naming the stray path and the profile dir, got %q", got)
	}
	if strings.Contains(got, "chmod") || strings.Contains(got, "0666") {
		t.Errorf("the Windows warning must not talk about chmod or POSIX modes, got %q", got)
	}

	// The POSIX branch is untouched by any of that.
	if got := loosePermWarning("linux", "/home/ada/.billkit/config.json", 0o600, "/home/ada"); got != "" {
		t.Errorf("a 0600 config warned on POSIX: %q", got)
	}
	if got := loosePermWarning("linux", "/home/ada/.billkit/config.json", 0o644, "/home/ada"); !strings.Contains(got, "chmod 600") {
		t.Errorf("a 0644 config must still warn on POSIX, got %q", got)
	}
	// A path outside $HOME on POSIX is fine as long as the mode is tight:
	// BILLKIT_CONFIG_HOME pointing at a 0600 file in /etc is legitimate.
	if got := loosePermWarning("linux", "/etc/billkit/config.json", 0o600, "/home/ada"); got != "" {
		t.Errorf("POSIX must judge the mode, not the location: %q", got)
	}
}

func TestValidateBaseURL(t *testing.T) {
	good := []string{"https://api.billkit.eu", "http://localhost:8000", "https://self.example/base"}
	for _, raw := range good {
		if err := ValidateBaseURL(raw); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v, want nil", raw, err)
		}
	}
	bad := []string{"", "api.billkit.eu", "file:///etc/passwd", "ftp://x.example", "https://user:pw@x.example"}
	for _, raw := range bad {
		if err := ValidateBaseURL(raw); err == nil {
			t.Errorf("ValidateBaseURL(%q) = nil, want an error", raw)
		}
	}
}

// TestCheckTransportRefusesLiveKeysOverCleartext pins the rule that a live
// key never crosses a channel that can be read or redirected.
func TestCheckTransportRefusesLiveKeysOverCleartext(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		apiKey  string
		wantErr bool
	}{
		{"live over https", "https://api.billkit.eu", "sk_live_x", false},
		{"live over http loopback", "http://127.0.0.1:8000", "sk_live_x", true},
		{"live over http remote", "http://attacker.example", "sk_live_x", true},
		{"test over http loopback", "http://127.0.0.1:8000", "sk_test_x", false},
		{"test over http localhost", "http://localhost:8000", "sk_test_x", false},
		{"test over http ipv6 loopback", "http://[::1]:8000", "sk_test_x", false},
		{"test over http remote", "http://attacker.example", "sk_test_x", true},
		{"test over https", "https://self.example", "sk_test_x", false},
		{"unknown key over http loopback", "http://127.0.0.1:8000", "opaque-token", true},
		{"garbage host", "not-a-url", "sk_test_x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckTransport(c.baseURL, c.apiKey)
			if c.wantErr && err == nil {
				t.Fatalf("CheckTransport(%q, %q) = nil, want an error", c.baseURL, c.apiKey)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("CheckTransport(%q, %q) = %v, want nil", c.baseURL, c.apiKey, err)
			}
		})
	}
}

func TestResolveRejectsAMalformedStoredBaseURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)

	cfg := &Config{
		DefaultProfile: "live",
		Profiles:       map[string]Profile{"live": {APIKey: "sk_live_x", BaseURL: "attacker.example"}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = loaded.Resolve("")
	if err == nil {
		t.Fatal("expected an error for a base_url that isn't a URL")
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "config.json")) {
		t.Fatalf("error should name the config file, got %v", err)
	}
}

func TestSetDefault(t *testing.T) {
	cfg := &Config{Profiles: map[string]Profile{}}
	cfg.Set("live", Profile{APIKey: "sk_live_y"})
	cfg.Set("test", Profile{APIKey: "sk_test_x"})
	if cfg.DefaultProfile != "live" {
		t.Fatalf("default = %q, want live (first Set)", cfg.DefaultProfile)
	}
	if !cfg.SetDefault("test") {
		t.Fatal("SetDefault(test) returned false")
	}
	if cfg.DefaultProfile != "test" {
		t.Fatalf("default = %q, want test", cfg.DefaultProfile)
	}
	if cfg.SetDefault("nope") {
		t.Fatal("SetDefault(nope) returned true for an unknown profile")
	}
	if cfg.DefaultProfile != "test" {
		t.Fatalf("a failed SetDefault changed the default to %q", cfg.DefaultProfile)
	}
}
