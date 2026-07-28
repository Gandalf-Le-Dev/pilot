package release

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Swap atomically points the service's `current` symlink at a release.
//
// The two-step dance — create a temporary symlink, then rename it over the
// real one — matters: rename(2) is atomic, so there is never a moment when
// `current` is missing or half-written. A reader either sees the old release
// or the new one.
func Swap(serviceDir, id string) error {
	if !IsID(id) {
		return fmt.Errorf("malformed release id %q", id)
	}

	relDir := filepath.Join(serviceDir, ReleasesDir, id)
	fi, err := os.Stat(relDir)
	if err != nil {
		return fmt.Errorf("release %s is not staged: %w", id, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("release %s is not a directory", id)
	}

	current := filepath.Join(serviceDir, CurrentLink)
	// If something replaced `current` with a real directory, swapping would
	// silently move the new symlink *inside* it. Refuse instead: a human needs
	// to look at this.
	if fi, err := os.Lstat(current); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s exists but is not a symlink; refusing to replace it", current)
	}

	pending := filepath.Join(serviceDir, PendingLink)
	if err := os.Remove(pending); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing stale %s: %w", PendingLink, err)
	}
	if err := os.Symlink(CurrentTarget(id), pending); err != nil {
		return err
	}
	if err := os.Rename(pending, current); err != nil {
		os.Remove(pending)
		return fmt.Errorf("activating %s: %w", id, err)
	}
	return nil
}

// ReadCurrent returns the live release ID, or "" when nothing is activated yet.
func ReadCurrent(serviceDir string) (string, error) {
	target, err := os.Readlink(filepath.Join(serviceDir, CurrentLink))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return filepath.Base(target), nil
}

// ListReleases returns the release IDs staged on disk, oldest first.
//
// Entries that don't parse as release IDs are ignored rather than reported:
// they're somebody's scratch directory, and Pilot has no business deleting or
// activating them.
func ListReleases(serviceDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(serviceDir, ReleasesDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && IsID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return SortIDs(ids), nil
}

// SelectForGC decides which releases to delete: everything outside the newest
// `keep`, except those named in protect (the current and previous releases),
// which are retained however old they are.
//
// Pure, so the policy is testable without a filesystem.
func SelectForGC(ids []string, keep int, protect ...string) []string {
	if keep < 1 {
		keep = 1 // never let a config mistake delete everything
	}

	retain := map[string]bool{}
	for _, p := range protect {
		if p != "" {
			retain[p] = true
		}
	}

	sorted := SortIDs(ids)
	for i, n := len(sorted)-1, 0; i >= 0 && n < keep; i, n = i-1, n+1 {
		retain[sorted[i]] = true
	}

	var remove []string
	for _, id := range sorted {
		if !retain[id] {
			remove = append(remove, id)
		}
	}
	return remove
}

// GC removes superseded releases and returns what it deleted.
func GC(serviceDir string, keep int, protect ...string) ([]string, error) {
	ids, err := ListReleases(serviceDir)
	if err != nil {
		return nil, err
	}
	remove := SelectForGC(ids, keep, protect...)
	for _, id := range remove {
		if err := os.RemoveAll(filepath.Join(serviceDir, ReleasesDir, id)); err != nil {
			return nil, fmt.Errorf("removing release %s: %w", id, err)
		}
	}
	return remove, nil
}

// EnsureService creates the directory skeleton for a service.
func EnsureService(serviceDir string) error {
	return os.MkdirAll(filepath.Join(serviceDir, ReleasesDir), 0o755)
}

// WriteFileAtomic writes data to path via a temporary file in the same
// directory, then renames it into place, so a reader never observes a partial
// file and an interrupted write leaves the previous version intact.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename succeeds

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// Chmod explicitly: CreateTemp makes 0600, which is right for .env but not
	// for a manifest others need to read.
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadState loads a service's state.json, returning a fresh State when the file
// is absent. A missing state file is normal on first deploy and must not be an
// error — the symlink, not this file, is the source of truth.
func ReadState(serviceDir, service string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(serviceDir, StateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return NewState(service), nil
		}
		return nil, err
	}
	return UnmarshalState(b)
}

// WriteState persists a service's state.json.
func WriteState(serviceDir string, s *State) error {
	b, err := MarshalState(s)
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(serviceDir, StateFile), b, 0o644)
}

// ReadManifest loads one release's manifest.
func ReadManifest(serviceDir, id string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(serviceDir, ReleasesDir, id, ManifestFile))
	if err != nil {
		return nil, err
	}
	return UnmarshalManifest(b)
}

// WriteManifest persists a release's manifest.
func WriteManifest(serviceDir, id string, m *Manifest) error {
	b, err := MarshalManifest(m)
	if err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(serviceDir, ReleasesDir, id, ManifestFile), b, 0o644)
}

// Reconcile brings state.json back in line with what the symlink actually says.
// The filesystem wins: if someone swapped `current` by hand, or state.json was
// lost, the live release is whatever the link points at.
func Reconcile(serviceDir string, s *State) (changed bool, err error) {
	live, err := ReadCurrent(serviceDir)
	if err != nil {
		return false, err
	}
	if live == "" || live == s.Current {
		return false, nil
	}
	s.Promote(live)
	return true, nil
}

// SortedHistoryReleases returns the distinct releases mentioned in history,
// newest first — the candidate list for `pilot rollback --to`.
func SortedHistoryReleases(s *State) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range s.History {
		if r.Release != "" && !seen[r.Release] {
			seen[r.Release] = true
			out = append(out, r.Release)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, _, ei := ParseID(out[i])
		sj, _, ej := ParseID(out[j])
		if ei != nil || ej != nil {
			return false
		}
		return si > sj
	})
	return out
}
