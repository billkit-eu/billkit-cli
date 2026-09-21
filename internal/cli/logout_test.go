package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/billkit-eu/billkit-cli/internal/config"
)

func TestLogoutDefaultProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	seedProfiles(t, map[string]config.Profile{
		"test": {APIKey: "bk_test_x"},
		"live": {APIKey: "bk_live_y"},
	}, "test")

	// No --profile, no --all → removes the default ("test").
	if err := runLogout(io.Discard, io.Discard, &globals{}, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if _, ok := cfg.Profiles["test"]; ok {
		t.Fatal("default profile 'test' should have been removed")
	}
	// This used to assert the default became "live". Logging out of test is
	// the last moment that should hand you a live key.
	if cfg.DefaultProfile != "" {
		t.Fatalf("default = %q, want empty after logging out of the default", cfg.DefaultProfile)
	}
}

// TestLogoutNeverPromotesLive is the regression for the silent live
// promotion. Three profiles make the behaviour visible on its own: with only
// two, one survivor remains and Resolve falls back to it either way, so the
// old bug hid behind that fallback.
func TestLogoutNeverPromotesLive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	seedProfiles(t, map[string]config.Profile{
		"test":    {APIKey: "bk_test_x"},
		"live":    {APIKey: "bk_live_y"},
		"staging": {APIKey: "bk_test_z"},
	}, "test")

	if err := runLogout(io.Discard, io.Discard, &globals{}, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if cfg.DefaultProfile == "live" {
		t.Fatal("logging out of the test profile made the LIVE profile the default: the next command would spend real money")
	}
	if cfg.DefaultProfile != "" {
		t.Fatalf("default = %q, want empty: logout must not choose a profile for you", cfg.DefaultProfile)
	}
}

// TestLogoutAnnouncesASoleLiveSurvivor covers the case the cleared default
// cannot fix by itself: one profile left means Resolve uses it regardless, so
// the user has to be told, in as many words, that it is live.
func TestLogoutAnnouncesASoleLiveSurvivor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	seedProfiles(t, map[string]config.Profile{
		"test": {APIKey: "bk_test_x"},
		"live": {APIKey: "bk_live_y"},
	}, "test")

	var errBuf bytes.Buffer
	if err := runLogout(io.Discard, &errBuf, &globals{}, false); err != nil {
		t.Fatal(err)
	}
	got := errBuf.String()
	if !strings.Contains(got, "LIVE key") || !strings.Contains(got, `"live"`) {
		t.Fatalf("logging out of test left only a live profile and said nothing useful about it; stderr = %q", got)
	}
}

func TestLogoutAll(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	seedProfiles(t, map[string]config.Profile{
		"test": {APIKey: "bk_test_x"},
		"live": {APIKey: "bk_live_y"},
	}, "test")

	if err := runLogout(io.Discard, io.Discard, &globals{}, true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if len(cfg.Profiles) != 0 || cfg.DefaultProfile != "" {
		t.Fatalf("expected all profiles cleared, got %+v", cfg)
	}
}

func TestLogoutNamedProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	seedProfiles(t, map[string]config.Profile{
		"test": {APIKey: "bk_test_x"},
		"live": {APIKey: "bk_live_y"},
	}, "test")

	if err := runLogout(io.Discard, io.Discard, &globals{profile: "live"}, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if _, ok := cfg.Profiles["live"]; ok {
		t.Fatal("named profile 'live' should have been removed")
	}
	if cfg.DefaultProfile != "test" {
		t.Fatalf("default should stay 'test', got %q", cfg.DefaultProfile)
	}
}

func TestLogoutUnknownProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BILLKIT_CONFIG_HOME", dir)
	seedProfiles(t, map[string]config.Profile{"test": {APIKey: "bk_test_x"}}, "test")

	if err := runLogout(io.Discard, io.Discard, &globals{profile: "nope"}, false); err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
}

func seedProfiles(t *testing.T, profiles map[string]config.Profile, def string) {
	t.Helper()
	cfg := &config.Config{Profiles: profiles, DefaultProfile: def}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}
