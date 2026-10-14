package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// memKeyring is an in-memory OS keychain. TestMain installs one for the whole
// package, so no test here can ever write into the keychain of the machine
// running it.
type memKeyring struct {
	mu      sync.Mutex
	items   map[string]string
	failSet error
	failGet error
}

func newMemKeyring() *memKeyring { return &memKeyring{items: map[string]string{}} }

func (k *memKeyring) Get(service, account string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failGet != nil {
		return "", k.failGet
	}
	v, ok := k.items[service+"/"+account]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (k *memKeyring) Set(service, account, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failSet != nil {
		return k.failSet
	}
	k.items[service+"/"+account] = secret
	return nil
}

func (k *memKeyring) Delete(service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.items, service+"/"+account)
	return nil
}

func (k *memKeyring) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.items)
}

func useMemKeyring(t *testing.T) *memKeyring {
	t.Helper()
	k := newMemKeyring()
	t.Cleanup(SetKeyring(k))
	return k
}

func TestMain(m *testing.M) {
	SetKeyring(newMemKeyring())
	os.Exit(m.Run())
}

// readFile returns config.json under dir as a string.
func readFile(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestLiveKeysGoToTheKeychainAndTestKeysStayInTheFile is the storage rule:
// a key that can move real money is not left in a plain file when the OS
// offers somewhere better; a test key, which cannot, stays where CI can
// reach it.
func TestLiveKeysGoToTheKeychainAndTestKeysStayInTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)
	kr := useMemKeyring(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	live := cfg.StoreKey("live", "bk_live_SECRETLIVE", "", StoreOptions{AllowKeychain: true})
	test := cfg.StoreKey("test", "bk_test_SECRETTEST", "", StoreOptions{AllowKeychain: true})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if live.Storage != "keychain" || live.KeychainErr != nil {
		t.Fatalf("live key storage = %+v, want the keychain", live)
	}
	if test.Storage != "file" {
		t.Fatalf("test key storage = %+v, want the file", test)
	}

	onDisk := readFile(t, dir)
	if strings.Contains(onDisk, "bk_live_SECRETLIVE") {
		t.Fatalf("the live key is in the file:\n%s", onDisk)
	}
	if !strings.Contains(onDisk, "bk_test_SECRETTEST") {
		t.Fatalf("the test key should be in the file:\n%s", onDisk)
	}
	if kr.count() != 1 {
		t.Fatalf("keychain holds %d item(s), want 1", kr.count())
	}

	// Every command resolves through the keychain transparently.
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := loaded.Resolve("live")
	if err != nil || p.APIKey != "bk_live_SECRETLIVE" {
		t.Fatalf("Resolve(live) = %+v, %v", p, err)
	}
	if loaded.Profiles["live"].Mode() != "live" || loaded.Profiles["live"].Storage() != "keychain" {
		t.Fatalf("a keychain profile must read as live without touching the keychain: %+v", loaded.Profiles["live"])
	}
}

// TestInsecureStorageKeepsALiveKeyInTheFile is `login --insecure-storage`.
func TestInsecureStorageKeepsALiveKeyInTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)
	kr := useMemKeyring(t)

	cfg, _ := Load()
	res := cfg.StoreKey("live", "bk_live_PLAIN", "", StoreOptions{AllowKeychain: false})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if res.Storage != "file" || kr.count() != 0 {
		t.Fatalf("storage = %+v, keychain items = %d; want the file and nothing in the keychain", res, kr.count())
	}
	assertOwnerOnlyPerm(t, filepath.Join(dir, "config.json"), 0o600)
}

// TestAnUnavailableKeychainFallsBackToTheFile: a headless Linux box or a CI
// runner has no Secret Service. The key still has to be stored, at 0600, and
// the caller told why it is not in the keychain.
func TestAnUnavailableKeychainFallsBackToTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)
	kr := useMemKeyring(t)
	kr.failSet = errors.New("org.freedesktop.secrets was not provided by any .service files")

	cfg, _ := Load()
	res := cfg.StoreKey("live", "bk_live_FALLBACK", "", StoreOptions{AllowKeychain: true})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if res.Storage != "file" || res.KeychainErr == nil {
		t.Fatalf("want a file fallback that reports the keychain error, got %+v", res)
	}
	assertOwnerOnlyPerm(t, filepath.Join(dir, "config.json"), 0o600)
	loaded, _ := Load()
	if p, _, err := loaded.Resolve("live"); err != nil || p.APIKey != "bk_live_FALLBACK" {
		t.Fatalf("Resolve(live) = %+v, %v", p, err)
	}
}

// TestDeleteRemovesTheKeychainEntryOnSave: logging out of a keychain profile
// must not leave the secret behind in the keychain, and must not remove it
// before the file that refers to it is rewritten.
func TestDeleteRemovesTheKeychainEntryOnSave(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	captureWarnings(t)
	kr := useMemKeyring(t)

	cfg, _ := Load()
	cfg.StoreKey("live", "bk_live_GONE", "", StoreOptions{AllowKeychain: true})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if kr.count() != 1 {
		t.Fatalf("setup: keychain items = %d", kr.count())
	}
	cfg.Delete("live")
	if kr.count() != 1 {
		t.Fatal("Delete removed the keychain entry before the file stopped referring to it")
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if kr.count() != 0 {
		t.Fatalf("keychain still holds %d item(s) after logout", kr.count())
	}
}

// TestReplacingALiveKeyWithATestKeyDropsTheOldKeychainEntry: re-using a
// profile name for a key of the other kind must not strand the old secret.
func TestReplacingALiveKeyWithATestKeyDropsTheOldKeychainEntry(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	captureWarnings(t)
	kr := useMemKeyring(t)

	cfg, _ := Load()
	cfg.StoreKey("staging", "bk_live_OLD", "", StoreOptions{AllowKeychain: true})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	cfg.StoreKey("staging", "bk_test_NEW", "", StoreOptions{AllowKeychain: true})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if kr.count() != 0 {
		t.Fatalf("the replaced live key is still in the keychain (%d item(s))", kr.count())
	}
}

// TestAnUnreadableKeychainEntryIsACredentialError names the fix.
func TestAnUnreadableKeychainEntryIsACredentialError(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	captureWarnings(t)
	kr := useMemKeyring(t)

	cfg, _ := Load()
	cfg.StoreKey("live", "bk_live_X", "", StoreOptions{AllowKeychain: true})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	kr.failGet = errors.New("user canceled")
	_, _, err := cfg.Resolve("live")
	if !errors.Is(err, ErrKeychainRead) || !strings.Contains(err.Error(), "billkit login --profile live") {
		t.Fatalf("want ErrKeychainRead naming the fix, got %v", err)
	}
}

// TestUnknownKeysSurviveARewrite is the forward-compatibility half of the
// schema version. A newer billkit adds a field; this build rewrites the file
// (any login, logout or `config use` does); the field must still be there,
// at the top level and inside a profile, or a downgrade silently destroys
// settings the user made with the newer version.
func TestUnknownKeysSurviveARewrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)

	planted := `{
  "version": 1,
  "default_profile": "test",
  "future_top_level": {"theme": "dark", "n": 9007199254740993},
  "profiles": {
    "test": {"api_key": "bk_test_KEEP", "future_profile_field": ["a", "b"]}
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set("other", Profile{APIKey: "bk_test_OTHER"})
	if !cfg.SetDefault("other") {
		t.Fatal("SetDefault")
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFile(t, dir)), &got); err != nil {
		t.Fatal(err)
	}
	if top := string(got["future_top_level"]); !strings.Contains(top, `"dark"`) || !strings.Contains(top, "9007199254740993") {
		t.Fatalf("the unknown top-level key was not preserved verbatim: %s", got["future_top_level"])
	}
	var profiles map[string]map[string]json.RawMessage
	if err := json.Unmarshal(got["profiles"], &profiles); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(profiles["test"]["future_profile_field"]), `"b"`) {
		t.Fatalf("the unknown profile key was dropped: %s", got["profiles"])
	}
	if string(got["version"]) != "1" {
		t.Fatalf("version = %s, want 1", got["version"])
	}
	if string(got["default_profile"]) != `"other"` {
		t.Fatalf("the known fields were not updated: %s", got["default_profile"])
	}
}

// TestAnUnversionedConfigIsReadAndStampedOnSave: every 0.x config has no
// version key. It must load as-is, and the first rewrite labels it.
func TestAnUnversionedConfigIsReadAndStampedOnSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)
	legacy := `{"default_profile":"test","profiles":{"test":{"api_key":"bk_test_OLD"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 0 {
		t.Fatalf("Version = %d, want 0 for an unversioned file", cfg.Version)
	}
	if p, _, err := cfg.Resolve(""); err != nil || p.APIKey != "bk_test_OLD" {
		t.Fatalf("Resolve = %+v, %v", p, err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, dir), `"version": 1`) {
		t.Fatalf("the rewrite did not stamp the schema version:\n%s", readFile(t, dir))
	}
}

// TestANewerSchemaIsReadButNeverRewritten: a downgrade keeps working for
// reads, and refuses to rewrite a file whose fields it may misread.
func TestANewerSchemaIsReadButNeverRewritten(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)
	future := `{"version": 99, "default_profile":"test","profiles":{"test":{"api_key":"bk_test_FUTURE"}}}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(future), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if p, _, err := cfg.Resolve(""); err != nil || p.APIKey != "bk_test_FUTURE" {
		t.Fatalf("a newer config must still be readable: %+v, %v", p, err)
	}
	if err := cfg.Save(); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Save = %v, want ErrNewerSchema", err)
	}
	if got := readFile(t, dir); got != future {
		t.Fatalf("the newer file was modified:\n%s", got)
	}
}

// TestDefaultDirFollowsThePlatformConvention pins the location for every
// platform from any host.
func TestDefaultDirFollowsThePlatformConvention(t *testing.T) {
	home := func() (string, error) { return "/home/ada", nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{"linux default", "linux", nil, filepath.Join("/home/ada", ".config", "billkit")},
		{"macOS default, like the Stripe CLI", "darwin", nil, filepath.Join("/home/ada", ".config", "billkit")},
		{"XDG_CONFIG_HOME", "linux", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, filepath.Join("/xdg", "billkit")},
		{"XDG_CONFIG_HOME on macOS", "darwin", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, filepath.Join("/xdg", "billkit")},
		{"a relative XDG_CONFIG_HOME is ignored", "linux", map[string]string{"XDG_CONFIG_HOME": "rel"}, filepath.Join("/home/ada", ".config", "billkit")},
		{"windows APPDATA", "windows", map[string]string{"APPDATA": `C:\Users\ada\AppData\Roaming`}, filepath.Join(`C:\Users\ada\AppData\Roaming`, "billkit")},
		{"windows without APPDATA", "windows", nil, filepath.Join("/home/ada", "AppData", "Roaming", "billkit")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := defaultDir(tc.goos, env(tc.env), home)
			if err != nil || got != tc.want {
				t.Fatalf("defaultDir = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestBillkitConfigHomeStillWins: the override is kept, and it is also what
// switches migration off.
func TestBillkitConfigHomeStillWins(t *testing.T) {
	t.Setenv("BILLKIT_CONFIG_HOME", "/somewhere/else")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err := Path()
	if err != nil || got != filepath.Join("/somewhere/else", "config.json") {
		t.Fatalf("Path = %q, %v", got, err)
	}
}

// legacyHome sets up a $HOME holding a 0.x ~/.billkit/config.json and an
// XDG_CONFIG_HOME for the new location, and returns both directories.
func legacyHome(t *testing.T, body string) (home, xdg string) {
	t.Helper()
	home = t.TempDir()
	xdg = filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BILLKIT_CONFIG_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(home, ".billkit"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".billkit", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, xdg
}

// TestLegacyConfigIsMovedOnFirstLoad: every 0.x user has their keys in
// ~/.billkit/config.json. The first 1.0 run moves them to the new location
// atomically and owner-only, puts the live key in the keychain, deletes the
// old copy (a second copy of a live key is the leftover nobody cleans up),
// leaves a note where it was, and keeps the user's default and any unknown
// fields.
func TestLegacyConfigIsMovedOnFirstLoad(t *testing.T) {
	home, xdg := legacyHome(t, `{"default_profile":"test","extra_thing":true,"profiles":{`+
		`"test":{"api_key":"bk_test_MOVEME","base_url":"http://localhost:8000"},`+
		`"live":{"api_key":"bk_live_MOVEME"}}}`)
	warnings := captureWarnings(t)
	kr := useMemKeyring(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(xdg, "billkit", "config.json")
	oldPath := filepath.Join(home, ".billkit", "config.json")

	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old config is still there (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".billkit", legacyNoteName)); err != nil {
		t.Fatalf("no note left behind: %v", err)
	}
	assertOwnerOnlyPerm(t, newPath, 0o600)
	onDisk, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "bk_live_MOVEME") {
		t.Fatalf("the live key was copied into the new file instead of the keychain:\n%s", onDisk)
	}
	if !strings.Contains(string(onDisk), `"extra_thing": true`) {
		t.Fatalf("an unknown key was lost in the move:\n%s", onDisk)
	}
	if kr.count() != 1 {
		t.Fatalf("keychain items = %d, want the one live key", kr.count())
	}
	if cfg.DefaultProfile != "test" {
		t.Fatalf("default = %q, want the user's own choice, test", cfg.DefaultProfile)
	}
	if !strings.Contains(warnings.String(), "Moved your BillKit config") {
		t.Fatalf("the move was not announced: %q", warnings.String())
	}

	// And it is all still usable, through a fresh Load from the new place.
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if p, _, err := again.Resolve("live"); err != nil || p.APIKey != "bk_live_MOVEME" {
		t.Fatalf("Resolve(live) after the move = %+v, %v", p, err)
	}
	if p, _, err := again.Resolve("test"); err != nil || p.APIKey != "bk_test_MOVEME" || p.BaseURL != "http://localhost:8000" {
		t.Fatalf("Resolve(test) after the move = %+v, %v", p, err)
	}
}

// TestLegacyConfigIsNotMovedOverANewOne: once the new file exists it is the
// truth, and an old one lying around is not merged over it.
func TestLegacyConfigIsNotMovedOverANewOne(t *testing.T) {
	home, xdg := legacyHome(t, `{"default_profile":"test","profiles":{"test":{"api_key":"bk_test_OLD"}}}`)
	warnings := captureWarnings(t)
	if err := os.MkdirAll(filepath.Join(xdg, "billkit"), 0o700); err != nil {
		t.Fatal(err)
	}
	current := `{"version":1,"default_profile":"test","profiles":{"test":{"api_key":"bk_test_NEW"}}}`
	if err := os.WriteFile(filepath.Join(xdg, "billkit", "config.json"), []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if p, _, _ := cfg.Resolve(""); p.APIKey != "bk_test_NEW" {
		t.Fatalf("the old config won: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(home, ".billkit", "config.json")); err != nil {
		t.Fatalf("an old config that was not moved must be left alone: %v", err)
	}
	// But not silently: it may hold a live key in plain text.
	if !strings.Contains(warnings.String(), "pre-1.0 BillKit config is still at") {
		t.Fatalf("the leftover old config was not reported: %q", warnings.String())
	}
}

// TestLegacyConfigIsNotMovedUnderAnOverride: BILLKIT_CONFIG_HOME names its
// own directory; ~/.billkit is somebody else's business then.
func TestLegacyConfigIsNotMovedUnderAnOverride(t *testing.T) {
	home, _ := legacyHome(t, `{"default_profile":"test","profiles":{"test":{"api_key":"bk_test_OLD"}}}`)
	captureWarnings(t)
	t.Setenv("BILLKIT_CONFIG_HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Profiles) != 0 {
		t.Fatalf("the override's empty config picked up legacy profiles: %+v", cfg.Profiles)
	}
	if _, err := os.Stat(filepath.Join(home, ".billkit", "config.json")); err != nil {
		t.Fatalf("the legacy file was touched: %v", err)
	}
}
