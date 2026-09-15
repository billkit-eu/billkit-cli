// Package sign implements BillKit's webhook signature scheme
// (BillKit-Signature: t=<unix>,v1=<hex>) — HMAC-SHA256 over "{t}.{body}" —
// matching the server's sign_outbound and every SDK's verifier. `billkit
// listen` re-signs each forwarded event with a locally-generated secret so
// the developer's app verifies it with any BillKit SDK.
package sign

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
)

// Header builds the value for the BillKit-Signature header.
func Header(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(body)
	return "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// NewWebhookSecret mints an ephemeral bkwhsec_… secret for a listen session.
func NewWebhookSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "bkwhsec_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
