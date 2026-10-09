package statuspage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// Token is the bearer token a host presents with its reports.
//
// Derived rather than stored: the operator keeps one secret, and each host's
// token follows from it and the host's name. An agent receives only its own,
// so a compromised host can speak for itself and no other.
func Token(secret, host string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(host))
	return hex.EncodeToString(mac.Sum(nil))
}

// TokenHash is what the server keeps in place of a token, so its
// configuration (readable by the server process, and copied wherever its
// unit's credentials go) cannot be replayed as a report.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
