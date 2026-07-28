package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatAndParseID(t *testing.T) {
	tests := []struct {
		seq  int
		hash string
		want string
	}{
		{42, "9f3ac1bdeadbeef", "0042-9f3ac1b"},
		{1, "abc1234", "0001-abc1234"},
		{99999, "abc1234", "99999-abc1234"},
	}
	for _, tc := range tests {
		got := FormatID(tc.seq, tc.hash)
		if got != tc.want {
			t.Errorf("FormatID(%d, %q) = %q, want %q", tc.seq, tc.hash, got, tc.want)
		}
		seq, _, err := ParseID(got)
		if err != nil {
			t.Fatalf("ParseID(%q): %v", got, err)
		}
		if seq != tc.seq {
			t.Errorf("round trip lost the sequence: %d != %d", seq, tc.seq)
		}
	}
}

func TestParseIDRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "0042", "42-abc1234", "0042-XYZ1234", "0042_abc1234", "current"} {
		if _, _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) should have failed", bad)
		}
		if IsID(bad) {
			t.Errorf("IsID(%q) should be false", bad)
		}
	}
}

func TestNextSeq(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want int
	}{
		{"empty", nil, 1},
		{"sequential", []string{"0001-aaaaaaa", "0002-bbbbbbb"}, 3},
		{"gaps", []string{"0001-aaaaaaa", "0009-bbbbbbb"}, 10},
		{"unordered", []string{"0009-bbbbbbb", "0001-aaaaaaa"}, 10},
		{"ignores junk", []string{"0003-aaaaaaa", "scratch", "current"}, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextSeq(tc.ids); got != tc.want {
				t.Errorf("NextSeq(%v) = %d, want %d", tc.ids, got, tc.want)
			}
		})
	}
}

// Identical inputs must produce identical hashes regardless of the order the
// caller added them, or "nothing changed" would be invisible in a plan.
func TestHasherIsOrderIndependent(t *testing.T) {
	a := NewHasher()
	a.AddString("runtime", "compose")
	a.AddString("image", "sha256:abc")
	a.AddMap("env", map[string]string{"B": "2", "A": "1"})

	b := NewHasher()
	b.AddMap("env", map[string]string{"A": "1", "B": "2"})
	b.AddString("image", "sha256:abc")
	b.AddString("runtime", "compose")

	if a.Sum() != b.Sum() {
		t.Errorf("hash depends on insertion order:\n%s\n%s", a.Sum(), b.Sum())
	}
	if len(a.Short()) != ShortHashLen {
		t.Errorf("Short() = %q, want %d chars", a.Short(), ShortHashLen)
	}
}

func TestHasherDetectsChanges(t *testing.T) {
	base := func() *Hasher {
		h := NewHasher()
		h.AddString("runtime", "compose")
		h.AddMap("env", map[string]string{"LOG_LEVEL": "info"})
		return h
	}
	orig := base().Sum()

	changed := base()
	changed.AddMap("env", map[string]string{"LOG_LEVEL": "debug"})
	if changed.Sum() == orig {
		t.Error("a changed env value must change the hash")
	}

	added := base()
	added.AddString("route", "api.example.com")
	if added.Sum() == orig {
		t.Error("an added part must change the hash")
	}
}

// Length-prefixing the part names prevents ("ab","c") colliding with ("a","bc").
func TestHasherNameBoundaries(t *testing.T) {
	a := NewHasher()
	a.AddString("ab", "c")
	b := NewHasher()
	b.AddString("a", "bc")
	if a.Sum() == b.Sum() {
		t.Error("part names must not run together in the digest")
	}
}

func TestSelectForGC(t *testing.T) {
	ids := []string{"0001-aaaaaaa", "0002-bbbbbbb", "0003-ccccccc", "0004-ddddddd", "0005-eeeeeee"}

	tests := []struct {
		name    string
		keep    int
		protect []string
		want    []string
	}{
		{"keep newest three", 3, nil, []string{"0001-aaaaaaa", "0002-bbbbbbb"}},
		{"keep all", 10, nil, nil},
		{"keep one", 1, nil, []string{"0001-aaaaaaa", "0002-bbbbbbb", "0003-ccccccc", "0004-ddddddd"}},
		{
			"protected old release survives",
			2, []string{"0001-aaaaaaa"},
			[]string{"0002-bbbbbbb", "0003-ccccccc"},
		},
		{
			"zero keep is clamped, never wipes everything",
			0, nil,
			[]string{"0001-aaaaaaa", "0002-bbbbbbb", "0003-ccccccc", "0004-ddddddd"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectForGC(ids, tc.keep, tc.protect...)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("SelectForGC(keep=%d, protect=%v) = %v, want %v", tc.keep, tc.protect, got, tc.want)
			}
		})
	}
}

// stageService builds a service directory with the given releases staged.
func stageService(t *testing.T, ids ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "api")
	if err := EnsureService(dir); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := os.MkdirAll(filepath.Join(dir, ReleasesDir, id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSwapAndReadCurrent(t *testing.T) {
	dir := stageService(t, "0001-aaaaaaa", "0002-bbbbbbb")

	if got, err := ReadCurrent(dir); err != nil || got != "" {
		t.Fatalf("ReadCurrent before any swap = %q, %v; want empty", got, err)
	}

	if err := Swap(dir, "0001-aaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadCurrent(dir); got != "0001-aaaaaaa" {
		t.Errorf("ReadCurrent = %q, want 0001-aaaaaaa", got)
	}

	// Swapping again must replace, not nest.
	if err := Swap(dir, "0002-bbbbbbb"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadCurrent(dir); got != "0002-bbbbbbb" {
		t.Errorf("ReadCurrent after second swap = %q, want 0002-bbbbbbb", got)
	}

	// The symlink target stays relative so the tree can be relocated.
	target, err := os.Readlink(filepath.Join(dir, CurrentLink))
	if err != nil {
		t.Fatal(err)
	}
	if target != "releases/0002-bbbbbbb" {
		t.Errorf("symlink target = %q, want a relative path", target)
	}

	// No temporary link left behind.
	if _, err := os.Lstat(filepath.Join(dir, PendingLink)); !os.IsNotExist(err) {
		t.Error("current.tmp should not survive a successful swap")
	}
}

func TestSwapRoundTripRollback(t *testing.T) {
	dir := stageService(t, "0001-aaaaaaa", "0002-bbbbbbb")
	for _, id := range []string{"0001-aaaaaaa", "0002-bbbbbbb", "0001-aaaaaaa"} {
		if err := Swap(dir, id); err != nil {
			t.Fatalf("swap to %s: %v", id, err)
		}
		if got, _ := ReadCurrent(dir); got != id {
			t.Fatalf("after swap to %s, current = %s", id, got)
		}
	}
}

func TestSwapRefusals(t *testing.T) {
	t.Run("unstaged release", func(t *testing.T) {
		dir := stageService(t, "0001-aaaaaaa")
		if err := Swap(dir, "0009-zzzzzzz"); err == nil {
			t.Error("want an error for a malformed id")
		}
		if err := Swap(dir, "0009-fffffff"); err == nil {
			t.Error("want an error for a release that was never staged")
		}
	})

	// If `current` is a real directory, renaming a symlink onto it would move
	// the link *inside* it — the classic mv-without-T bug. Refuse loudly.
	t.Run("current is a real directory", func(t *testing.T) {
		dir := stageService(t, "0001-aaaaaaa")
		if err := os.Mkdir(filepath.Join(dir, CurrentLink), 0o755); err != nil {
			t.Fatal(err)
		}
		err := Swap(dir, "0001-aaaaaaa")
		if err == nil {
			t.Fatal("want a refusal")
		}
		if !strings.Contains(err.Error(), "not a symlink") {
			t.Errorf("error should say why, got: %v", err)
		}
	})

	t.Run("stale pending link is cleared", func(t *testing.T) {
		dir := stageService(t, "0001-aaaaaaa")
		if err := os.Symlink("releases/nonexistent", filepath.Join(dir, PendingLink)); err != nil {
			t.Fatal(err)
		}
		if err := Swap(dir, "0001-aaaaaaa"); err != nil {
			t.Fatalf("a leftover current.tmp should not block a swap: %v", err)
		}
	})
}

func TestListReleasesIgnoresStrays(t *testing.T) {
	dir := stageService(t, "0002-bbbbbbb", "0001-aaaaaaa")
	if err := os.MkdirAll(filepath.Join(dir, ReleasesDir, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ReleasesDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ListReleases(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0001-aaaaaaa", "0002-bbbbbbb"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ListReleases = %v, want %v (oldest first, strays ignored)", got, want)
	}
}

func TestGCProtectsLiveReleases(t *testing.T) {
	dir := stageService(t, "0001-aaaaaaa", "0002-bbbbbbb", "0003-ccccccc", "0004-ddddddd")
	if err := Swap(dir, "0004-ddddddd"); err != nil {
		t.Fatal(err)
	}

	st := NewState("api")
	st.Promote("0003-ccccccc")
	st.Promote("0004-ddddddd")

	removed, err := GC(dir, 2, st.Protected()...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(removed, ",") != "0001-aaaaaaa,0002-bbbbbbb" {
		t.Errorf("removed %v, want the two oldest", removed)
	}
	// Rollback must still be possible.
	if _, err := os.Stat(filepath.Join(dir, ReleasesDir, "0003-ccccccc")); err != nil {
		t.Error("previous release was deleted; rollback would be impossible")
	}
	if got, _ := ReadCurrent(dir); got != "0004-ddddddd" {
		t.Errorf("current changed during GC: %q", got)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m := &Manifest{
		Schema:     ManifestSchema,
		Service:    "api",
		Release:    "0042-9f3ac1b",
		Sequence:   42,
		Hash:       "9f3ac1bdeadbeef",
		Runtime:    "compose",
		Host:       "web-1",
		CreatedAt:  time.Now().UTC().Truncate(time.Second),
		DeployedBy: "me@laptop",
		Source:     &SourceRef{Repo: "git@github.com:me/api.git", Ref: "main", Commit: "abc123"},
		Artifacts: []Artifact{
			{Name: "image", Kind: ArtifactImage, Ref: "ghcr.io/me/api@sha256:abc", Digest: "sha256:abc"},
		},
		EnvKeys:   []string{"DATABASE_URL", "LOG_LEVEL"},
		EnvHash:   "deadbeef",
		RouteHash: "cafebabe",
	}

	b, err := MarshalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Release != m.Release || got.EnvHash != m.EnvHash || len(got.Artifacts) != 1 {
		t.Errorf("round trip lost data: %+v", got)
	}

	// The manifest is printed by `pilot releases` and may reach a log, so it
	// must never carry resolved secret values — only their names and a digest.
	if strings.Contains(string(b), "postgres://") {
		t.Error("manifest must not contain env values")
	}
	if !strings.Contains(string(b), "DATABASE_URL") {
		t.Error("manifest should record env keys")
	}
}

func TestManifestValidation(t *testing.T) {
	valid := func() *Manifest {
		return &Manifest{
			Schema: ManifestSchema, Service: "api",
			Release: "0042-9f3ac1b", Sequence: 42, Runtime: "compose",
		}
	}
	tests := []struct {
		name string
		mut  func(*Manifest)
		want string
	}{
		{"bad schema", func(m *Manifest) { m.Schema = 99 }, "schema"},
		{"no service", func(m *Manifest) { m.Service = "" }, "service"},
		{"no release", func(m *Manifest) { m.Release = "" }, "release id"},
		{"sequence disagrees", func(m *Manifest) { m.Sequence = 7 }, "disagrees"},
		{
			"undigested artifact",
			func(m *Manifest) { m.Artifacts = []Artifact{{Name: "image", Kind: ArtifactImage}} },
			"no digest",
		},
		{
			"image pinned to a tag is mutable, so rejected",
			func(m *Manifest) {
				m.Artifacts = []Artifact{{Name: "image", Kind: ArtifactImage, Ref: "ghcr.io/me/api:latest", Digest: "sha256:abc"}}
			},
			"not pinned to a digest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := valid()
			tc.mut(m)
			err := m.Validate()
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestStatePromoteAndRollback(t *testing.T) {
	s := NewState("api")
	if _, err := s.RollbackTarget(); err == nil {
		t.Error("a service with no history has nothing to roll back to")
	}

	s.Promote("0001-aaaaaaa")
	s.Promote("0002-bbbbbbb")
	if s.Current != "0002-bbbbbbb" || s.Previous != "0001-aaaaaaa" {
		t.Fatalf("state = %+v", s)
	}

	target, err := s.RollbackTarget()
	if err != nil || target != "0001-aaaaaaa" {
		t.Errorf("RollbackTarget = %q, %v", target, err)
	}

	// Re-activating the live release must not discard the rollback target.
	s.Promote("0002-bbbbbbb")
	if s.Previous != "0001-aaaaaaa" {
		t.Errorf("redundant promote destroyed previous: %+v", s)
	}
}

func TestStateHistoryIsCapped(t *testing.T) {
	s := NewState("api")
	for i := 1; i <= MaxHistory+10; i++ {
		s.Record(DeployRecord{Release: FormatID(i, "aaaaaaa"), Outcome: OutcomeOK})
	}
	if len(s.History) != MaxHistory {
		t.Errorf("history = %d entries, want it capped at %d", len(s.History), MaxHistory)
	}
	if last := s.LastDeploy(); last == nil || last.Release != FormatID(MaxHistory+10, "aaaaaaa") {
		t.Errorf("history should be newest-first, got %+v", s.History[0])
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := stageService(t, "0001-aaaaaaa")

	// A missing state file is normal on first deploy.
	s, err := ReadState(dir, "api")
	if err != nil {
		t.Fatalf("absent state.json must not be an error: %v", err)
	}
	s.Promote("0001-aaaaaaa")
	s.Record(DeployRecord{
		Release: "0001-aaaaaaa", Host: "web-1", By: "me",
		StartedAt: time.Now().UTC(), Outcome: OutcomeOK,
	})
	if err := WriteState(dir, s); err != nil {
		t.Fatal(err)
	}

	back, err := ReadState(dir, "api")
	if err != nil {
		t.Fatal(err)
	}
	if back.Current != "0001-aaaaaaa" || len(back.History) != 1 {
		t.Errorf("round trip lost data: %+v", back)
	}
}

// The filesystem is the source of truth: if state.json is lost or someone
// swapped the symlink by hand, Pilot believes the symlink.
func TestReconcileFollowsTheSymlink(t *testing.T) {
	dir := stageService(t, "0001-aaaaaaa", "0002-bbbbbbb")
	if err := Swap(dir, "0002-bbbbbbb"); err != nil {
		t.Fatal(err)
	}

	s := NewState("api")
	s.Promote("0001-aaaaaaa") // stale belief

	changed, err := Reconcile(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Reconcile should have noticed the divergence")
	}
	if s.Current != "0002-bbbbbbb" || s.Previous != "0001-aaaaaaa" {
		t.Errorf("state = %+v, want current to follow the symlink", s)
	}

	if changed, _ := Reconcile(dir, s); changed {
		t.Error("Reconcile should be idempotent once in sync")
	}
}

func TestWriteFileAtomicPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")

	if err := WriteFileAtomic(path, []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600 for a file holding secrets", perm)
	}

	// Overwriting leaves no temporary files behind.
	if err := WriteFileAtomic(path, []byte("SECRET=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want just the target file", len(entries))
	}
	b, _ := os.ReadFile(path)
	if string(b) != "SECRET=2\n" {
		t.Errorf("content = %q", b)
	}
}

func TestLayoutPaths(t *testing.T) {
	l := NewLayout("")
	if l.Root != DefaultRoot {
		t.Errorf("empty root should default to %s, got %s", DefaultRoot, l.Root)
	}

	tests := []struct{ got, want string }{
		{l.Service("api"), "/opt/pilot/services/api"},
		{l.Releases("api"), "/opt/pilot/services/api/releases"},
		{l.Release("api", "0042-9f3ac1b"), "/opt/pilot/services/api/releases/0042-9f3ac1b"},
		{l.Current("api"), "/opt/pilot/services/api/current"},
		{l.Pending("api"), "/opt/pilot/services/api/current.tmp"},
		{l.State("api"), "/opt/pilot/services/api/state.json"},
		{l.Manifest("api", "0042-9f3ac1b"), "/opt/pilot/services/api/releases/0042-9f3ac1b/manifest.json"},
		{l.Env("api", "0042-9f3ac1b"), "/opt/pilot/services/api/releases/0042-9f3ac1b/.env"},
		{l.Agent(), "/opt/pilot/bin/pilotd"},
		{CurrentTarget("0042-9f3ac1b"), "releases/0042-9f3ac1b"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
