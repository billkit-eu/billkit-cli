package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// accounts lists the keychain accounts the fake keyring holds.
func (k *memKeyring) accounts() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for key := range k.items {
		out = append(out, strings.TrimPrefix(key, KeyringService+"/"))
	}
	slices.Sort(out)
	return out
}

func (k *memKeyring) value(account string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.items[KeyringService+"/"+account]
}

// referencedOnDisk lists the keychain accounts the config file at path names.
func referencedOnDisk(t *testing.T, path string) []string {
	t.Helper()
	cfg, err := loadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range cfg.Profiles {
		if p.Keychain != "" {
			out = append(out, p.Keychain)
		}
	}
	slices.Sort(out)
	return out
}

// skipWithoutWritableModes bails out where a read-only directory cannot be
// built: Windows ignores the POSIX bits, and root ignores them everywhere.
func skipWithoutWritableModes(t *testing.T) {
	t.Helper()
	skipWithoutPOSIXModes(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory")
	}
}

// readOnlyDir makes dir unwritable for the rest of the test.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func runConcurrently(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			errs[i] = fn(i)
		})
	}
	close(start)
	wg.Wait()
	return errs
}

// TestConcurrentFirstRunsMoveTheLegacyConfigOnce: two billkit processes
// started together on a 0.x config each moved it, each put the live key in
// the keychain under a fresh account, and the losing account was referenced
// by nothing, a live key stranded in the keychain for good.
func TestConcurrentFirstRunsMoveTheLegacyConfigOnce(t *testing.T) {
	home, xdg := legacyHome(t, `{"default_profile":"live","profiles":{"live":{"api_key":"bk_live_RACE"}}}`)
	captureWarnings(t)
	kr := useMemKeyring(t)

	for i, err := range runConcurrently(8, func(int) error {
		cfg, err := Load()
		if err == nil && cfg.Profiles["live"].Keychain == "" {
			err = errors.New("a Load returned the live profile without its keychain reference")
		}
		return err
	}) {
		if err != nil {
			t.Fatalf("Load %d: %v", i, err)
		}
	}
	newPath := filepath.Join(xdg, "billkit", "config.json")
	got, want := kr.accounts(), referencedOnDisk(t, newPath)
	if len(got) != 1 || !slices.Equal(got, want) {
		t.Fatalf("keychain accounts = %v, the file references %v; want exactly one, referenced", got, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".billkit", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old config is still there (%v)", err)
	}
}

// TestConcurrentLoginsLeaveOneKeychainEntry is the same race through login:
// without the lock each process read the same file, stored its own account
// and wrote, and every account but the last writer's leaked.
func TestConcurrentLoginsLeaveOneKeychainEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)
	kr := useMemKeyring(t)

	for i, err := range runConcurrently(8, func(i int) error {
		return Update(func(c *Config) error {
			c.StoreKey("live", fmt.Sprintf("bk_live_LOGIN%d", i), "", StoreOptions{AllowKeychain: true})
			return nil
		})
	}) {
		if err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
	}
	got, want := kr.accounts(), referencedOnDisk(t, filepath.Join(dir, "config.json"))
	if len(got) != 1 || !slices.Equal(got, want) {
		t.Fatalf("keychain accounts = %v, the file references %v; want exactly one, referenced", got, want)
	}
}

// TestConcurrentLoginsToDifferentProfilesAllSurvive: the other half of a
// lost update. Eight logins to eight profiles leave eight profiles.
func TestConcurrentLoginsToDifferentProfilesAllSurvive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)
	kr := useMemKeyring(t)

	runConcurrently(8, func(i int) error {
		return Update(func(c *Config) error {
			c.StoreKey(fmt.Sprintf("p%d", i), fmt.Sprintf("bk_live_P%d", i), "", StoreOptions{AllowKeychain: true})
			return nil
		})
	})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Profiles) != 8 || kr.count() != 8 {
		t.Fatalf("profiles = %v, keychain items = %d; want 8 of each", cfg.ProfileNames(), kr.count())
	}
}

// TestAFailedMoveIsNeverSavedInPlainTextAtTheNewPath: when the new location
// cannot be written (a root-owned ~/.config), the old config is used where it
// is. A later save used to write it, live key and all, in plain text at the
// new path, and the old file then stayed forever because the new one existed.
func TestAFailedMoveIsNeverSavedInPlainTextAtTheNewPath(t *testing.T) {
	skipWithoutWritableModes(t)
	home, xdg := legacyHome(t, `{"default_profile":"live","profiles":{"live":{"api_key":"bk_live_STUCK"}}}`)
	warnings := captureWarnings(t)
	kr := useMemKeyring(t)
	readOnlyDir(t, xdg)
	oldPath := filepath.Join(home, ".billkit", "config.json")
	newPath := filepath.Join(xdg, "billkit", "config.json")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if p, _, err := cfg.Resolve("live"); err != nil || p.APIKey != "bk_live_STUCK" {
		t.Fatalf("the old config must still work in place: %+v, %v", p, err)
	}
	if kr.count() != 0 {
		t.Fatalf("a failed move left %d keychain item(s) behind", kr.count())
	}
	if !strings.Contains(warnings.String(), "Could not move your BillKit config") {
		t.Fatalf("the failed move was not reported: %q", warnings.String())
	}

	if err := Update(func(c *Config) error {
		c.StoreKey("test", "bk_test_LATER", "", StoreOptions{AllowKeychain: true})
		return nil
	}); err != nil {
		t.Fatalf("a save while the move cannot finish must still work: %v", err)
	}
	if _, err := os.Lstat(newPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a config was written at the new path (%v)", err)
	}
	old, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(old), "bk_test_LATER") {
		t.Fatalf("the save did not go back to the old location:\n%s", old)
	}
	if !strings.Contains(warnings.String(), "Saved it at its old location instead") {
		t.Fatalf("saving to the old location was not reported: %q", warnings.String())
	}

	// Once the new location is writable, the next run finishes the move
	// properly: live key in the keychain, old file gone.
	if err := os.Chmod(xdg, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "bk_live_STUCK") || !strings.Contains(string(onDisk), "bk_test_LATER") {
		t.Fatalf("the finished move wrote:\n%s", onDisk)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old config is still there (%v)", err)
	}
	if kr.count() != 1 {
		t.Fatalf("keychain items = %d, want the one live key", kr.count())
	}
}

// TestSavingALegacyConfigFinishesTheMove: a config read in place whose save
// finds the new location writable completes the whole move rather than
// copying the file across as it stands.
func TestSavingALegacyConfigFinishesTheMove(t *testing.T) {
	skipWithoutWritableModes(t)
	home, xdg := legacyHome(t, `{"default_profile":"live","profiles":{"live":{"api_key":"bk_live_FINISH"}}}`)
	captureWarnings(t)
	kr := useMemKeyring(t)
	readOnlyDir(t, xdg)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(xdg, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.SetDefault("live")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(xdg, "billkit", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "bk_live_FINISH") {
		t.Fatalf("the live key was written in plain text at the new path:\n%s", onDisk)
	}
	if _, err := os.Stat(filepath.Join(home, ".billkit", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old config is still there (%v)", err)
	}
	if kr.count() != 1 {
		t.Fatalf("keychain items = %d, want 1", kr.count())
	}
}

// TestANewerSchemaPutsNothingInTheKeychain: the save is going to be refused,
// so the key must not be stored first and left behind.
func TestANewerSchemaPutsNothingInTheKeychain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)
	kr := useMemKeyring(t)
	future := `{"version": 99, "profiles":{}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(future), 0o600); err != nil {
		t.Fatal(err)
	}

	called := false
	err := Update(func(c *Config) error {
		called = true
		c.StoreKey("live", "bk_live_NEWER", "", StoreOptions{AllowKeychain: true})
		return nil
	})
	if !errors.Is(err, ErrNewerSchema) || called {
		t.Fatalf("Update = %v (callback ran: %v), want ErrNewerSchema before the callback", err, called)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.StoreKey("live", "bk_live_NEWER", "", StoreOptions{AllowKeychain: true})
	if kr.count() != 0 {
		t.Fatal("StoreKey put a key in the keychain for a config that can never be saved")
	}
	if err := cfg.Save(); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Save = %v, want ErrNewerSchema", err)
	}
	if kr.count() != 0 {
		t.Fatalf("keychain items = %d after a refused save, want 0", kr.count())
	}
}

// TestAFailedSaveKeepsTheOldKeyAndDropsTheNewEntry: logging in again to a
// keychain profile used to overwrite the stored key in place before the save,
// so a save that then failed (read-only directory, full disk) left the file
// pointing at an account that now held a key the user never saw saved.
func TestAFailedSaveKeepsTheOldKeyAndDropsTheNewEntry(t *testing.T) {
	skipWithoutWritableModes(t)
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	captureWarnings(t)
	kr := useMemKeyring(t)

	if err := Update(func(c *Config) error {
		c.StoreKey("live", "bk_live_FIRST", "", StoreOptions{AllowKeychain: true})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := kr.accounts()
	readOnlyDir(t, dir)

	err := Update(func(c *Config) error {
		c.StoreKey("live", "bk_live_SECOND", "", StoreOptions{AllowKeychain: true})
		return nil
	})
	if err == nil {
		t.Fatal("a save into a read-only directory succeeded")
	}
	if got := kr.accounts(); !slices.Equal(got, before) || kr.value(before[0]) != "bk_live_FIRST" {
		t.Fatalf("keychain = %v (%q), want %v still holding the first key", got, kr.value(before[0]), before)
	}
	if !slices.Equal(referencedOnDisk(t, filepath.Join(dir, "config.json")), before) {
		t.Fatal("the file no longer references the surviving entry")
	}
}

// TestAnInterruptedMoveIsFinished: a crash between writing the new file and
// deleting the old one left the old plain-text copy forever, since the new
// file's existence switched the move off.
func TestAnInterruptedMoveIsFinished(t *testing.T) {
	home, xdg := legacyHome(t, `{"default_profile":"test","profiles":{`+
		`"test":{"api_key":"bk_test_HALF"},"live":{"api_key":"bk_live_HALF"}}}`)
	warnings := captureWarnings(t)
	kr := useMemKeyring(t)
	if err := kr.Set(KeyringService, "live.0000", "bk_live_HALF"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(xdg, "billkit"), 0o700); err != nil {
		t.Fatal(err)
	}
	moved := `{"version":1,"default_profile":"test","profiles":{` +
		`"test":{"api_key":"bk_test_HALF"},"live":{"keychain_account":"live.0000"}}}`
	if err := os.WriteFile(filepath.Join(xdg, "billkit", "config.json"), []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".billkit", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the half-moved old config is still there (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".billkit", legacyNoteName)); err != nil {
		t.Fatalf("no note left behind: %v", err)
	}
	if !strings.Contains(warnings.String(), "Finished moving your BillKit config") {
		t.Fatalf("finishing the move was not announced: %q", warnings.String())
	}
}

// TestLoadReadOnlyMovesNothing: shell completion reads the profile names on
// every Tab press, and must not be what moves a user's keys.
func TestLoadReadOnlyMovesNothing(t *testing.T) {
	home, xdg := legacyHome(t, `{"default_profile":"live","profiles":{"live":{"api_key":"bk_live_TAB"}}}`)
	captureWarnings(t)
	kr := useMemKeyring(t)

	cfg, err := LoadReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ProfileNames(), []string{"live"}) {
		t.Fatalf("profiles = %v", cfg.ProfileNames())
	}
	if kr.count() != 0 {
		t.Fatal("a read-only load wrote to the keychain")
	}
	if _, err := os.Stat(filepath.Join(home, ".billkit", "config.json")); err != nil {
		t.Fatalf("a read-only load moved the old config: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(xdg, "billkit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a read-only load created the config directory (%v)", err)
	}
}
