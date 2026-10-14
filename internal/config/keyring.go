package config

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	gokeyring "github.com/zalando/go-keyring"
)

// KeyringService is the service name every billkit keychain entry is filed
// under. It is what a user sees in Keychain Access, Windows Credential
// Manager or Seahorse, so it names the tool plainly.
const KeyringService = "billkit-cli"

// keyringTimeout bounds every keychain call. On Linux the Secret Service can
// sit waiting for an unlock prompt that a headless session will never show,
// and a CLI that hangs at login is worse than one that falls back to the file
// and says so.
const keyringTimeout = 10 * time.Second

// ErrKeychainRead is a stored profile whose key the keychain would not give
// back. It is a credential failure, so the command layer reports it as one.
var ErrKeychainRead = errors.New("could not read the API key from the OS keychain")

// errKeyringTimeout is a keychain call that did not answer in time.
var errKeyringTimeout = errors.New("the OS keychain did not respond")

// Keyring is the slice of an OS credential store the CLI uses.
type Keyring interface {
	Get(service, account string) (string, error)
	Set(service, account, secret string) error
	Delete(service, account string) error
}

// osKeyring is the platform store: macOS Keychain (through /usr/bin/security,
// fed on stdin so the secret never appears in argv), Windows Credential
// Manager, or the Secret Service over D-Bus on Linux. All three are pure Go,
// so the binary stays CGO-free.
type osKeyring struct{}

func (osKeyring) Get(service, account string) (string, error) {
	return withTimeout(func() (string, error) { return gokeyring.Get(service, account) })
}

func (osKeyring) Set(service, account, secret string) error {
	_, err := withTimeout(func() (string, error) { return "", gokeyring.Set(service, account, secret) })
	return err
}

func (osKeyring) Delete(service, account string) error {
	_, err := withTimeout(func() (string, error) { return "", gokeyring.Delete(service, account) })
	if errors.Is(err, gokeyring.ErrNotFound) {
		return nil
	}
	return err
}

// withTimeout runs one keychain call with a deadline. The goroutine is left
// behind when the deadline wins; the process is about to fall back or exit,
// and there is no way to cancel a D-Bus prompt from here anyway.
func withTimeout(call func() (string, error)) (string, error) {
	type result struct {
		value string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		v, err := call()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-time.After(keyringTimeout):
		return "", errKeyringTimeout
	}
}

var (
	keyringMu sync.Mutex
	keyring   Keyring = osKeyring{}
)

// SetKeyring replaces the credential store and returns a function that puts
// the previous one back. It exists for tests, which must never write into
// the real keychain of the machine running them, and for an embedding caller
// with its own store.
func SetKeyring(k Keyring) (restore func()) {
	keyringMu.Lock()
	prev := keyring
	keyring = k
	keyringMu.Unlock()
	return func() {
		keyringMu.Lock()
		keyring = prev
		keyringMu.Unlock()
	}
}

func currentKeyring() Keyring {
	keyringMu.Lock()
	defer keyringMu.Unlock()
	return keyring
}

func keyringGet(account string) (string, error) {
	return currentKeyring().Get(KeyringService, account)
}

// StoreOptions controls where StoreKey may put a key.
type StoreOptions struct {
	// AllowKeychain lets a live key go to the OS keychain. False is
	// `billkit login --insecure-storage`: the key goes to the 0600 file.
	AllowKeychain bool
}

// StoreResult says where StoreKey put the key.
type StoreResult struct {
	// Storage is "keychain" or "file".
	Storage string
	// KeychainErr is why a live key that should have gone to the keychain
	// went to the file instead. Nil when nothing fell back.
	KeychainErr error
}

// StoreKey stores key under the profile name with the given base URL.
//
// A live key goes to the OS keychain when that is allowed and the keychain
// accepts it, and the profile records only the keychain account. Everything
// else goes in the file: a test key, because it cannot move money and a CI
// runner has no keychain to put it in, and a live key the keychain refused,
// which is reported back in StoreResult.KeychainErr so the caller can say so.
//
// Nothing is written to disk here. The keychain write happens immediately,
// because the profile is about to refer to it, and always under a fresh
// account: overwriting the account the profile already names would destroy
// the old key before the file that replaces it is safely written. The old
// account is deleted by Save once the new file is in place; if the save
// fails, Save deletes the new account instead and the old key is untouched.
// A config a newer billkit wrote is never saved, so for one of those nothing
// goes to the keychain at all.
func (c *Config) StoreKey(name, key, baseURL string, opts StoreOptions) StoreResult {
	prev, existed := c.Profiles[name]
	p := Profile{BaseURL: baseURL}
	if existed {
		p.extra = prev.extra
	}

	res := StoreResult{Storage: "file"}
	if Mode(key) == "live" && opts.AllowKeychain && c.checkWritable() == nil {
		account := newKeychainAccount(name)
		if err := currentKeyring().Set(KeyringService, account, key); err != nil {
			res.KeychainErr = err
		} else {
			p.Keychain = account
			c.created = append(c.created, account)
			res.Storage = "keychain"
		}
	}
	if p.Keychain == "" {
		p.APIKey = key
	}
	if prev.Keychain != "" && prev.Keychain != p.Keychain {
		c.orphans = append(c.orphans, prev.Keychain)
	}
	c.Set(name, p)
	return res
}

// newKeychainAccount names a fresh keychain entry. The profile name keeps it
// recognisable in Keychain Access; the random suffix keeps two config
// directories (BILLKIT_CONFIG_HOME) that both have a "live" profile from
// overwriting each other's key, and lets StoreKey write a replacement key
// without touching the one it replaces.
func newKeychainAccount(profile string) string {
	buf := make([]byte, 4)
	if _, err := cryptorand.Read(buf); err != nil {
		return fmt.Sprintf("%s.%d", profile, time.Now().UnixNano())
	}
	return profile + "." + hex.EncodeToString(buf)
}

// referenced is the set of keychain accounts the profiles name.
func (c *Config) referenced() map[string]bool {
	out := map[string]bool{}
	for _, p := range c.Profiles {
		if p.Keychain != "" {
			out[p.Keychain] = true
		}
	}
	return out
}

// commit runs after a successful save: every keychain entry this config
// orphaned, and every one it created, that the file just written does not
// reference is deleted. Best effort: a failure leaves a stale entry behind,
// which is reported, and never undoes the save.
func (c *Config) commit() {
	ref := c.referenced()
	var drop []string
	for _, account := range append(c.orphans, c.created...) {
		if !ref[account] {
			drop = append(drop, account)
		}
	}
	deleteKeychainAccounts(drop)
	c.orphans, c.created = nil, nil
}

// rollback runs after a failed or abandoned save. The old file is still the
// one on disk, so the orphans it references must stay, and the entries made
// for the new one are reachable from nothing and are deleted.
func (c *Config) rollback() {
	deleteKeychainAccounts(c.created)
	c.created = nil
}

func deleteKeychainAccounts(accounts []string) {
	for _, account := range accounts {
		if err := currentKeyring().Delete(KeyringService, account); err != nil {
			warnf("! Could not delete the API key %q (service %q) from the OS keychain: %v\n  Remove it by hand.\n",
				account, KeyringService, err)
		}
	}
}
