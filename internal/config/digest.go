package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// digestHeader opens a pushed host-wide config. A YAML comment, so the agent's
// strict parse never sees it and the digest is stored atomically with the
// config it describes: there is no second file to fall out of step.
const digestHeader = "# pilot fleet config digest: "

// SpecDigest is the digest of a rendered host-wide config.
func SpecDigest(spec string) string {
	sum := sha256.Sum256([]byte(spec))
	return hex.EncodeToString(sum[:])[:16]
}

// StampDigest prefixes a spec with its digest line.
func StampDigest(spec, digest string) string {
	return digestHeader + digest + "\n" + spec
}

// StampedDigest reads the digest line from a spec, or "" when it has none,
// as a config pushed by a pilot that predates the stamp does not.
func StampedDigest(spec string) string {
	line, _, _ := strings.Cut(spec, "\n")
	d, ok := strings.CutPrefix(line, digestHeader)
	if !ok {
		return ""
	}
	return strings.TrimSpace(d)
}
