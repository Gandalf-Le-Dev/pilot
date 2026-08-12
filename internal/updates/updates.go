// Package updates reports newer versions of the images a fleet runs.
//
// It reads the image references Pilot already knows from each service's compose
// file, asks the registry what tags exist, and compares. Nothing is deployed,
// nothing is pulled, and no host is touched — this is a read against public
// registry metadata, so it works from a laptop with no fleet access at all.
package updates

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/Gandalf-Le-Dev/pilot/internal/composefile"
	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/registry"
)

// Result is one image's standing against its registry.
type Result struct {
	Service string `json:"service"`
	Image   string `json:"image"`
	Current string `json:"current"`
	Latest  string `json:"latest,omitempty"`

	// Step is patch, minor, major, or current — and empty when no comparison
	// happened at all. That distinction is load-bearing for a machine reader:
	// reporting "current" for a tag that was never checked is a claim this did
	// not make, and a script acting on it would treat an unchecked image as a
	// verified one.
	Step registry.Step `json:"step,omitempty"`

	// Track marks a tag that names a moving series rather than one release,
	// like `postgres:17`. Those are not compared: they are chosen precisely so
	// they move, and reporting the next major every day is how a checker
	// becomes something people mute.
	Track bool `json:"track,omitempty"`

	// Err explains why this image could not be checked. A private repository
	// or a rate limit must say so rather than read as "no updates available".
	Err string `json:"error,omitempty"`
}

// Outdated reports whether a real update is waiting.
func (r Result) Outdated() bool {
	return r.Err == "" && !r.Track && r.Latest != "" && r.Step != registry.StepNone
}

// Lister fetches tags. An interface so tests need no network.
type Lister interface {
	Tags(ctx context.Context, ref registry.Ref) ([]string, error)
}

// Check inspects every image the fleet's services declare.
func Check(ctx context.Context, fleet *config.Fleet, lister Lister) []Result {
	type job struct {
		service string
		image   string
	}

	var jobs []job
	for _, name := range fleet.ServiceNames() {
		s := fleet.Services[name]
		for _, image := range imagesFor(fleet.Root, s) {
			jobs = append(jobs, job{service: name, image: image})
		}
	}

	results := make([]Result, len(jobs))
	var wg sync.WaitGroup

	// Bounded: a fleet with many services should not open a connection per
	// image at once, and registries rate-limit by client.
	sem := make(chan struct{}, 6)

	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = check(ctx, lister, j.service, j.image)
		}(i, j)
	}
	wg.Wait()

	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Service != results[j].Service {
			return results[i].Service < results[j].Service
		}
		return results[i].Image < results[j].Image
	})
	return results
}

func check(ctx context.Context, lister Lister, service, image string) Result {
	// Step stays empty until a comparison actually happens.
	out := Result{Service: service, Image: image}

	ref, err := registry.ParseRef(image)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	out.Image = ref.Name()
	out.Current = ref.Tag

	current, ok := registry.ParseVersion(ref.Tag)
	if !ok {
		// `latest` and friends. There is no version to compare, and the
		// existing image-tags check already has opinions about those.
		out.Err = "tag is not a version"
		return out
	}
	if current.IsTrack() {
		out.Track = true
		return out
	}

	tags, err := lister.Tags(ctx, ref)
	if err != nil {
		if errors.Is(err, registry.ErrUnauthorized) {
			out.Err = "registry denied access (private image?)"
		} else {
			out.Err = err.Error()
		}
		return out
	}

	latest, found := registry.Latest(current, tags)
	if !found {
		out.Step = registry.StepNone
		return out
	}
	out.Latest = latest.Raw
	out.Step = current.StepTo(latest)
	return out
}

// imagesFor extracts the image references a service declares.
//
// Only compose services have them; a systemd service ships binaries and a
// static one ships files, and neither has an upstream release to track.
func imagesFor(root string, s *config.Service) []string {
	if s.Runtime != config.RuntimeCompose || s.Compose == nil {
		return nil
	}

	body, err := composefile.Read(root, s)
	if err != nil {
		return nil
	}
	return composefile.Images(body)
}
