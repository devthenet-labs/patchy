// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
)

// pairwiseVersion starts every pairwise input, so a later construction never
// collides with this one.
const pairwiseVersion = "patchy-preview-auth/v1/sub"

// Sub is the pairwise subject of username on one Preview through one client:
// base64url(HMAC-SHA256(pairwise key, fields)), 43 characters, under the
// current generation. The fields are length-prefixed, so no two distinct
// inputs share an encoding. It is stable for one viewer on one Preview, and
// says nothing about the viewer to anyone without the key: not the login, and
// not whether two Previews' viewers are the same person.
//
// It is computed once, when a code is issued, and carried in the code and
// the refresh token after that, so a key rotation never changes a session's
// subject.
func (r *KeyRing) Sub(clientID, previewUID, label, username string) string {
	mac := hmac.New(sha256.New, r.current.pairwise)
	for _, field := range []string{pairwiseVersion, clientID, previewUID, label, username} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(field)))
		mac.Write(n[:])
		mac.Write([]byte(field))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
