package updates

import (
	"context"
	"strings"
	"testing"

	"github.com/Gandalf-Le-Dev/pilot/internal/registry"
)

// fakeLister answers from a table, and records what it was asked, so a test can
// assert that a lookup was skipped rather than merely uninteresting.
type fakeLister struct {
	tags  map[string][]string
	err   error
	asked []string
}

func (f *fakeLister) Tags(_ context.Context, ref registry.Ref) ([]string, error) {
	f.asked = append(f.asked, ref.Name())
	if f.err != nil {
		return nil, f.err
	}
	return f.tags[ref.Name()], nil
}

func TestCheckReportsAnUpdate(t *testing.T) {
	f := &fakeLister{tags: map[string][]string{
		"ghcr.io/paperless-ngx/paperless-ngx": {"3.0.3", "3.0.4", "3.0.5", "latest", "fix-13627"},
	}}

	got := check(context.Background(), f, "paperless", "ghcr.io/paperless-ngx/paperless-ngx:3.0.4")

	if !got.Outdated() {
		t.Fatalf("want an update, got %+v", got)
	}
	if got.Latest != "3.0.5" || got.Step != registry.StepPatch {
		t.Errorf("got %s (%s), want 3.0.5 (patch)", got.Latest, got.Step)
	}
}

// A moving tag is a deliberate choice. Comparing it would mean announcing the
// next major every day for a migration nobody does on a Tuesday — and it must
// not even cost a registry round-trip.
func TestCheckSkipsMovingTagsWithoutAsking(t *testing.T) {
	f := &fakeLister{tags: map[string][]string{"postgres": {"14", "15", "16", "17", "18"}}}

	got := check(context.Background(), f, "atuin", "postgres:14")

	if !got.Track {
		t.Errorf("postgres:14 should be reported as a moving tag, got %+v", got)
	}
	if got.Outdated() {
		t.Error("a moving tag must never count as an available update")
	}
	if len(f.asked) != 0 {
		t.Errorf("the registry was queried for a tag that is not compared: %v", f.asked)
	}
}

// The failure that would make this feature dangerous: losing access to a
// registry and reporting silence, which reads exactly like "nothing to do".
func TestCheckReportsDeniedAccessRatherThanSilence(t *testing.T) {
	f := &fakeLister{err: registry.ErrUnauthorized}

	got := check(context.Background(), f, "kite", "ghcr.io/gandalf-le-dev/kite:0.1.4")

	if got.Err == "" {
		t.Fatal("a registry denial must be reported, not swallowed")
	}
	if !strings.Contains(got.Err, "denied") {
		t.Errorf("Err = %q, want it to name the access problem", got.Err)
	}
	if got.Outdated() {
		t.Error("a failed check must not be reported as an update")
	}
}

// `latest` has no version to compare against, and saying so beats implying the
// image is current.
func TestCheckRejectsNonVersionTags(t *testing.T) {
	f := &fakeLister{}
	got := check(context.Background(), f, "x", "nginx:latest")

	if got.Err == "" {
		t.Error("a non-version tag should be reported as uncheckable")
	}
	if len(f.asked) != 0 {
		t.Errorf("no registry lookup should happen for %q: %v", "latest", f.asked)
	}
}

func TestCheckUpToDate(t *testing.T) {
	f := &fakeLister{tags: map[string][]string{
		"docmost/docmost": {"0.94.0", "0.95.0"},
	}}

	got := check(context.Background(), f, "docmost", "docmost/docmost:0.95.0")

	if got.Outdated() || got.Err != "" {
		t.Errorf("want up to date, got %+v", got)
	}
	if got.Step != registry.StepNone {
		t.Errorf("Step = %q, want %q", got.Step, registry.StepNone)
	}
}

// A digest names exact bits and belongs to no series, which is the reason
// somebody pinned one. It must not be reported as an update or as an error
// people learn to ignore.
func TestCheckDigestPinned(t *testing.T) {
	f := &fakeLister{}
	digest := "ghcr.io/x/y@sha256:" + strings.Repeat("a", 64)

	got := check(context.Background(), f, "x", digest)

	if got.Err == "" {
		t.Error("a digest reference should explain why it cannot be compared")
	}
	if len(f.asked) != 0 {
		t.Errorf("no lookup should happen for a digest: %v", f.asked)
	}
}

// An unchecked image must not claim to be current. A machine reader acting on
// `step: current` for a tag nobody compared would treat an unverified image as
// a verified one — which is the whole failure mode this feature exists to
// avoid, reproduced in its own output.
func TestUncheckedImagesReportNoStep(t *testing.T) {
	f := &fakeLister{err: registry.ErrUnauthorized}

	for name, res := range map[string]Result{
		"moving tag": check(context.Background(), &fakeLister{}, "x", "postgres:14"),
		"denied":     check(context.Background(), f, "x", "ghcr.io/private/app:1.0.0"),
		"latest":     check(context.Background(), &fakeLister{}, "x", "nginx:latest"),
	} {
		if res.Step != "" {
			t.Errorf("%s: Step = %q, want empty — nothing was compared", name, res.Step)
		}
	}

	// A genuine comparison that found nothing newer does say so.
	ok := check(context.Background(), &fakeLister{tags: map[string][]string{
		"docmost/docmost": {"0.95.0"},
	}}, "docmost", "docmost/docmost:0.95.0")
	if ok.Step != registry.StepNone {
		t.Errorf("a real comparison should report %q, got %q", registry.StepNone, ok.Step)
	}
}
