// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// MaxSlots is the most preview slots an installation can have; it matches the
// chart's preview.slotCount maximum and the four guard slot namespaces.
const MaxSlots = 4

// clientPrefix starts every slot's client id. The client id is public: the
// ALB shows it to preview code in x-amzn-oidc-data anyway.
const clientPrefix = "patchy-preview-s"

// clientSecretInfo separates the client-secret construction from every other
// use of the master secret.
const clientSecretInfo = "patchy-preview-auth/v1/client-secret"

// ClientID is slot's ALB client id, patchy-preview-s<slot>.
func ClientID(slot int) string { return clientPrefix + strconv.Itoa(slot) }

// ALBCookieName is slot's ALB session cookie name (the ALB appends -0,
// -1, ... to it). It is distinct from every cookie patchy sets itself and from
// the ALB's default, and has no __Host- prefix because the ALB's cookie Path
// and Domain are not documented.
func ALBCookieName(slot int) string { return clientPrefix + strconv.Itoa(slot) }

// SlotOf returns the slot of clientID when it is exactly ClientID(s) for some
// s in [0, slotCount). A non-canonical number (a sign, a leading zero, spaces)
// is not a client.
func SlotOf(clientID string, slotCount int) (int, bool) {
	digits, ok := strings.CutPrefix(clientID, clientPrefix)
	if !ok || slotCount < 1 || slotCount > MaxSlots {
		return 0, false
	}
	slot, ok := canonicalUint(digits)
	if !ok || slot >= slotCount {
		return 0, false
	}
	return slot, true
}

// clientSecret is hex(sha256(master || "\x00" || info || "\x00" || gen ||
// "\x00" || slot)), the construction the chart reproduces with sha256sum. A
// length extension appends bytes after the slot, so it never yields a
// canonical integer and matches no slot.
func clientSecret(master []byte, gen, slot int) string {
	h := sha256.New()
	h.Write(master)
	h.Write([]byte("\x00" + clientSecretInfo + "\x00" + strconv.Itoa(gen) + "\x00" + strconv.Itoa(slot)))
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalUint parses s as a non-negative decimal with no sign, no leading
// zero (other than "0" itself) and at most six digits.
func canonicalUint(s string) (int, bool) {
	if s == "" || len(s) > 6 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}
