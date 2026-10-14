package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// legacyNoteName is the file left where the pre-1.0 config used to be.
const legacyNoteName = "MOVED.txt"

// legacyPath is where every 0.x release kept the config: ~/.billkit.
func legacyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".billkit", "config.json"), nil
}

// legacyCandidate returns the pre-1.0 config path when one exists and could
// matter for path: nothing has relocated the config (BILLKIT_CONFIG_HOME is
// unset) and the old file is not the new one.
func legacyCandidate(path string) (string, bool) {
	if os.Getenv(configHomeEnv) != "" {
		return "", false
	}
	old, err := legacyPath()
	if err != nil || filepath.Clean(old) == filepath.Clean(path) {
		return "", false
	}
	if _, err := os.Lstat(old); err != nil {
		return "", false
	}
	return old, true
}

// loadLegacy reads the pre-1.0 file where it is, marked so that Save never
// writes it anywhere but back there or, by finishing the move, to the new
// location with its live keys in the keychain.
func loadLegacy(old string) (*Config, error) {
	cfg, err := loadFrom(old)
	if err != nil {
		return nil, err
	}
	cfg.path = old
	cfg.legacy = true
	return cfg, nil
}

// migrateLegacy moves a pre-1.0 ~/.billkit/config.json to path, once. The
// caller holds the config lock, which is what makes "once" true when several
// billkit processes start at the same moment: each re-checks the conditions
// here, under the lock, and only the first finds the new file missing.
//
// It runs only when nothing has relocated the config, nothing is at the new
// location yet, and the old file exists. The move itself is
// completeMigration.
//
// moved is true when the returned config came from the old file. When the
// move fails the old file is left exactly as it was, the error is reported on
// stderr, and the config is returned marked legacy: the command still works,
// a Save writes back to the old location rather than putting a live key in
// plain text at the new one, and the next run tries the move again.
func migrateLegacy(path string) (cfg *Config, moved bool) {
	old, ok := legacyCandidate(path)
	if !ok {
		return nil, false
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	legacy, err := loadLegacy(old)
	if err != nil {
		warnf("! Could not move your BillKit config from %s to %s: %v\n", old, path, err)
		return nil, false
	}
	if err := legacy.completeMigration(path); err != nil {
		warnf("! Could not move your BillKit config from %s to %s: %v\n"+
			"  Using it where it is for now. Nothing is written to the new location until\n"+
			"  the move can finish, and it is tried again on the next run.\n", old, path, err)
	}
	return legacy, true
}

// completeMigration finishes moving a legacy config to newPath: each live key
// still in the old file goes to the OS keychain where that works (the same
// rule `login` follows), the new file is written through the same atomic 0600
// path every save uses, and only then is the old file deleted, with a note
// left beside it saying where the config went. Deleting it is the point: a
// second copy of a live key in a directory nothing reads any more is exactly
// the kind of leftover nobody remembers to clean up.
//
// The work is done on a copy. If the new file cannot be written, the keychain
// entries made for it are deleted again and c is unchanged, so it still holds
// the keys the old file holds and can be saved back there.
func (c *Config) completeMigration(newPath string) error {
	old := c.path
	next := *c
	next.Profiles = maps.Clone(c.Profiles)
	var created, fellBack []string
	for _, name := range next.ProfileNames() {
		p := next.Profiles[name]
		if p.Keychain != "" || Mode(p.APIKey) != "live" {
			continue
		}
		account := newKeychainAccount(name)
		if err := currentKeyring().Set(KeyringService, account, p.APIKey); err != nil {
			fellBack = append(fellBack, name)
			continue
		}
		created = append(created, account)
		p.Keychain, p.APIKey = account, ""
		next.Profiles[name] = p
	}
	if err := next.writeTo(newPath); err != nil {
		deleteKeychainAccounts(created)
		return err
	}
	c.Profiles, c.Version, c.path, c.legacy = next.Profiles, next.Version, newPath, false
	retireLegacy(old, newPath, "Moved")
	if len(fellBack) > 0 {
		warnf("! The OS keychain was not available, so the live key(s) for %v stay in %s (mode 0600).\n", fellBack, newPath)
	}
	return nil
}

// retireLegacy deletes the old file once its contents are safely at newPath,
// and leaves a note where it was.
func retireLegacy(old, newPath, verb string) {
	note := fmt.Sprintf("billkit 1.0 moved the config that was here to\n\n  %s\n\n"+
		"on %s. Live API keys were moved into the OS keychain where one was available.\n"+
		"Nothing reads this directory any more, and it can be deleted.\n",
		newPath, time.Now().UTC().Format(time.RFC3339))
	// The note is a courtesy; failing to write it must not fail the move.
	_ = os.WriteFile(filepath.Join(filepath.Dir(old), legacyNoteName), []byte(note), fileMode)
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		warnf("! %s your BillKit config to %s, but could not delete the old copy at %s: %v\n  It still holds your API keys; delete it by hand.\n", verb, newPath, old, err)
		return
	}
	syncDir(filepath.Dir(old))
	// A `!` line, not a `>` notice: scripts that read the old path stop
	// finding it, so this is said even under --json.
	warnf("! %s your BillKit config from %s to %s.\n", verb, old, newPath)
}

// bothExistOnce keeps the "two configs" warning to one line per process.
var bothExistOnce sync.Once

// finishInterruptedMove handles a pre-1.0 file that is still there although
// the new one exists and no MOVED.txt says the move happened: a run that
// crashed between writing the new file and deleting the old one. Left alone,
// that file keeps a plain-text copy of every key in it forever, in a
// directory nothing reads.
//
// When every profile in the old file is in the new one with the same key and
// host, the move had finished in all but the delete, so the delete is finished now. Anything
// else is not this CLI's to decide (the user may have put the old file back
// on purpose), so it says so instead. With a MOVED.txt present the old file
// is something a 0.x billkit wrote after the move, and it is left alone. The
// caller holds the config lock.
func finishInterruptedMove(path string) {
	old, ok := legacyCandidate(path)
	if !ok {
		return
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(old), legacyNoteName)); err == nil {
		return
	}
	cur, err := loadFrom(path)
	if err != nil {
		return
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		return
	}
	legacy, err := loadFrom(old)
	if err == nil {
		for _, name := range legacy.ProfileNames() {
			if !sameCredential(legacy.Profiles[name], cur.Profiles[name]) {
				err = fmt.Errorf("profile %q differs between the two", name)
				break
			}
		}
	}
	if err != nil {
		bothExistOnce.Do(func() {
			warnf("! A pre-1.0 BillKit config is still at %s, and it may hold API keys in plain\n"+
				"  text. billkit reads only %s (%v).\n"+
				"  Copy anything you still need with `billkit login`, then delete the old file.\n", old, path, err)
		})
		return
	}
	retireLegacy(old, path, "Finished moving")
}

// sameCredential reports whether cur holds exactly the credential old does:
// the same host and the same key, where a live key the move put in the
// keychain is compared with what the keychain gives back for it.
func sameCredential(old, cur Profile) bool {
	if old.BaseURL != cur.BaseURL {
		return false
	}
	switch {
	case old.Keychain != "":
		return cur.Keychain == old.Keychain
	case cur.Keychain == "":
		return cur.APIKey == old.APIKey
	default:
		key, err := keyringGet(cur.Keychain)
		return err == nil && key == old.APIKey
	}
}
