package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestHeaderMatchesBillKitScheme(t *testing.T) {
	secret := "bkwhsec_unit_test_secret"
	body := []byte(`{"id":"evt_1","type":"customer.created"}`)
	var ts int64 = 1_700_000_000

	got := Header(secret, ts, body)

	// Independently recompute the documented scheme: HMAC-SHA256 over
	// "{t}." + body, hex, in a "t=<unix>,v1=<hex>" header.
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	want := "t=1700000000,v1=" + hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Fatalf("Header() = %q, want %q", got, want)
	}
}

func TestNewWebhookSecretIsPrefixedAndUnique(t *testing.T) {
	a, err := NewWebhookSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a, "bkwhsec_") {
		t.Fatalf("missing bkwhsec_ prefix: %q", a)
	}
	b, _ := NewWebhookSecret()
	if a == b {
		t.Fatal("expected two distinct secrets")
	}
}
