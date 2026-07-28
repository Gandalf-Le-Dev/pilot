package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ShortHashLen is how much of the content hash appears in a release ID. Seven
// hex characters is the git convention and is plenty to disambiguate the few
// releases a service keeps.
const ShortHashLen = 7

// SeqDigits is the zero-padded width of the sequence number, chosen so that
// release directories sort lexically in deploy order for the first 10k deploys.
const SeqDigits = 4

var idPattern = regexp.MustCompile(`^(\d{4,})-([0-9a-f]{7,64})$`)

// FormatID builds a release ID from a sequence number and a content hash.
// The sequence gives humans an ordering; the hash tells them whether two
// releases are actually the same bits.
func FormatID(seq int, hash string) string {
	if len(hash) > ShortHashLen {
		hash = hash[:ShortHashLen]
	}
	return fmt.Sprintf("%0*d-%s", SeqDigits, seq, hash)
}

// ParseID splits a release ID back into its sequence and short hash.
func ParseID(id string) (seq int, hash string, err error) {
	m := idPattern.FindStringSubmatch(id)
	if m == nil {
		return 0, "", fmt.Errorf("malformed release id %q: want <seq>-<hash>, e.g. 0042-9f3ac1b", id)
	}
	seq, err = strconv.Atoi(m[1])
	if err != nil {
		return 0, "", fmt.Errorf("malformed release id %q: %w", id, err)
	}
	return seq, m[2], nil
}

// IsID reports whether s looks like a release ID, used to ignore stray files
// when listing a releases directory.
func IsID(s string) bool { return idPattern.MatchString(s) }

// NextSeq returns the sequence number for the next release given the IDs that
// already exist. Unparseable entries are ignored rather than fatal: a human
// may have left a directory behind, and that shouldn't block a deploy.
func NextSeq(existing []string) int {
	max := 0
	for _, id := range existing {
		seq, _, err := ParseID(id)
		if err != nil {
			continue
		}
		if seq > max {
			max = seq
		}
	}
	return max + 1
}

// SortIDs orders release IDs oldest-first by sequence number.
func SortIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool {
		si, _, ei := ParseID(out[i])
		sj, _, ej := ParseID(out[j])
		switch {
		case ei != nil && ej != nil:
			return out[i] < out[j]
		case ei != nil:
			return true // unparseable sorts first, so GC reaps it soonest
		case ej != nil:
			return false
		}
		return si < sj
	})
	return out
}

// Hasher accumulates the inputs that define a release and reduces them to a
// stable digest. Two releases built from identical inputs get identical
// hashes, which is what makes "nothing actually changed" visible in a plan.
//
// Entries are sorted before hashing, so callers don't have to add them in a
// fixed order to get a reproducible result.
type Hasher struct {
	parts map[string]string
}

func NewHasher() *Hasher { return &Hasher{parts: map[string]string{}} }

// Add records a named blob. Later Adds with the same name replace earlier ones.
func (h *Hasher) Add(name string, data []byte) {
	sum := sha256.Sum256(data)
	h.parts[name] = hex.EncodeToString(sum[:])
}

// AddString records a named scalar.
func (h *Hasher) AddString(name, value string) { h.Add(name, []byte(value)) }

// AddMap records a name/value map, hashing keys and values together so a
// changed value is detected without the value itself being retained.
func (h *Hasher) AddMap(name string, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, m[k])
	}
	h.AddString(name, b.String())
}

// Sum returns the full hex digest over every recorded part.
func (h *Hasher) Sum() string {
	names := make([]string, 0, len(h.parts))
	for n := range h.parts {
		names = append(names, n)
	}
	sort.Strings(names)

	outer := sha256.New()
	for _, n := range names {
		// Length-prefix the name so that ("ab","c") and ("a","bc") can't
		// collide into the same byte stream.
		fmt.Fprintf(outer, "%d:%s=%s\n", len(n), n, h.parts[n])
	}
	return hex.EncodeToString(outer.Sum(nil))
}

// Short returns the leading ShortHashLen characters of Sum.
func (h *Hasher) Short() string { return h.Sum()[:ShortHashLen] }

// HashMap is a convenience for hashing one map, used for env and route digests.
func HashMap(m map[string]string) string {
	h := NewHasher()
	h.AddMap("m", m)
	return h.Sum()
}

// HashBytes is a convenience for hashing one blob.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
