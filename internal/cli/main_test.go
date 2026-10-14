package cli

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/billkit-eu/billkit-cli/internal/config"
)

// fakeKeyring is an in-memory OS keychain. Every test in this package runs
// against one, installed by TestMain, because a test that logs in with a
// bk_live_ key would otherwise write it into the real keychain of whoever
// runs `go test`.
type fakeKeyring struct {
	mu      sync.Mutex
	items   map[string]string
	failSet error // when set, Set fails with it (a keychain that is not there)
}

func newFakeKeyring() *fakeKeyring { return &fakeKeyring{items: map[string]string{}} }

func (k *fakeKeyring) Get(service, account string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.items[service+"/"+account]
	if !ok {
		return "", errors.New("secret not found in keyring")
	}
	return v, nil
}

func (k *fakeKeyring) Set(service, account, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.failSet != nil {
		return k.failSet
	}
	k.items[service+"/"+account] = secret
	return nil
}

func (k *fakeKeyring) Delete(service, account string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.items, service+"/"+account)
	return nil
}

func (k *fakeKeyring) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.items)
}

// useFakeKeyring gives one test its own empty keyring.
func useFakeKeyring(t *testing.T) *fakeKeyring {
	t.Helper()
	k := newFakeKeyring()
	restore := config.SetKeyring(k)
	t.Cleanup(restore)
	return k
}

func TestMain(m *testing.M) {
	config.SetKeyring(newFakeKeyring())
	os.Exit(m.Run())
}
