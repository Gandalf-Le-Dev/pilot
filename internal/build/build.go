// Package build produces the artifacts a release ships.
//
// Builds run on the operator's machine or come from CI — never on a target
// host. Building on a production box competes for CPU with the thing it serves
// and fills its disk with layers and node_modules, which is a failure that
// arrives at the worst possible moment.
package build

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Gandalf-Le-Dev/pilot/internal/config"
	"github.com/Gandalf-Le-Dev/pilot/internal/release"
)

// Result is what a build produced.
type Result struct {
	// Dir holds the files to ship, assembled on the local machine. Empty when
	// the release is only an image reference.
	Dir string

	// Cleanup removes any temporary directory created for Dir.
	Cleanup func()

	Artifacts []release.Artifact
	Commit    string
}

// Source locates a service's code.
type Source struct {
	// Dir is the checkout to build in.
	Dir string
	// Commit is the resolved revision, empty when the source is not a git
	// repository.
	Commit string
}

// Resolve locates the source tree for a service.
//
// A `path:` is used as-is, which is the common case when services live in the
// same repository as the fleet configuration. A `repo:` is cloned into a cache
// directory and fetched on subsequent runs.
func Resolve(ctx context.Context, s *config.Service, fleetRoot, cacheDir string) (Source, error) {
	if s.Source == nil {
		return Source{Dir: fleetRoot}, nil
	}

	if s.Source.Path != "" {
		dir := s.Source.Path
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(fleetRoot, dir)
		}
		if _, err := os.Stat(dir); err != nil {
			return Source{}, fmt.Errorf("source path for %s: %w", s.Name, err)
		}
		commit, _ := gitRev(ctx, dir)
		return Source{Dir: dir, Commit: commit}, nil
	}

	if s.Source.Repo == "" {
		return Source{Dir: fleetRoot}, nil
	}

	dir := filepath.Join(cacheDir, s.Name)
	ref := s.Source.Ref
	if ref == "" {
		ref = "HEAD"
	}

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return Source{}, err
		}
		if out, err := run(ctx, "", "git", "clone", "--quiet", s.Source.Repo, dir); err != nil {
			return Source{}, fmt.Errorf("cloning %s: %w\n%s", s.Source.Repo, err, out)
		}
	} else if out, err := run(ctx, dir, "git", "fetch", "--quiet", "--all", "--prune"); err != nil {
		return Source{}, fmt.Errorf("fetching %s: %w\n%s", s.Source.Repo, err, out)
	}

	if out, err := run(ctx, dir, "git", "checkout", "--quiet", "--force", ref); err != nil {
		return Source{}, fmt.Errorf("checking out %s in %s: %w\n%s", ref, s.Name, err, out)
	}
	// Fast-forward a branch checkout; a tag or SHA has no upstream and that is
	// not an error.
	_, _ = run(ctx, dir, "git", "merge", "--ff-only", "--quiet", "@{u}")

	commit, err := gitRev(ctx, dir)
	if err != nil {
		return Source{}, err
	}
	return Source{Dir: dir, Commit: commit}, nil
}

func gitRev(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Build runs a service's build and collects its outputs.
func Build(ctx context.Context, s *config.Service, src Source, log func(string, ...any)) (*Result, error) {
	res := &Result{Commit: src.Commit, Cleanup: func() {}}

	if s.Build == nil {
		// Nothing to build. A compose service can legitimately reference
		// images that CI already published.
		if s.Runtime == config.RuntimeStatic {
			return nil, fmt.Errorf("service %q has no build, but a static site needs one", s.Name)
		}
		return res, nil
	}

	if s.Build.Command != "" {
		log("building %s", s.Name)
		if out, err := run(ctx, src.Dir, "sh", "-c", s.Build.Command); err != nil {
			return nil, fmt.Errorf("build failed for %s: %w\n%s", s.Name, err, out)
		}
	}

	if len(s.Build.Output) > 0 {
		dir, artifacts, err := collect(src.Dir, s.Build.Output)
		if err != nil {
			return nil, err
		}
		res.Dir = dir
		res.Cleanup = func() { os.RemoveAll(dir) }
		res.Artifacts = append(res.Artifacts, artifacts...)
	}

	if s.Build.Image != "" {
		art, err := buildImage(ctx, s, src, log)
		if err != nil {
			res.Cleanup()
			return nil, err
		}
		res.Artifacts = append(res.Artifacts, art)
	}

	return res, nil
}

// buildImage builds and pushes a container image, then resolves it to a digest.
//
// Pinning to a digest is what makes a release immutable: a tag can be moved
// under you, so a release referencing one would not describe fixed bits.
func buildImage(ctx context.Context, s *config.Service, src Source, log func(string, ...any)) (release.Artifact, error) {
	tag := s.Build.Image + ":" + shortCommit(src.Commit)

	buildCtx := s.Build.Context
	if buildCtx == "" {
		buildCtx = "."
	}
	args := []string{"build", "--tag", tag}
	if s.Build.Dockerfile != "" {
		args = append(args, "--file", s.Build.Dockerfile)
	}
	args = append(args, buildCtx)

	log("building image %s", tag)
	if out, err := run(ctx, src.Dir, "docker", args...); err != nil {
		return release.Artifact{}, fmt.Errorf("docker build failed for %s: %w\n%s", s.Name, err, out)
	}

	log("pushing %s", tag)
	if out, err := run(ctx, src.Dir, "docker", "push", "--quiet", tag); err != nil {
		return release.Artifact{}, fmt.Errorf("docker push failed for %s: %w\n%s", s.Name, err, out)
	}

	digest, err := imageDigest(ctx, src.Dir, tag)
	if err != nil {
		return release.Artifact{}, err
	}

	return release.Artifact{
		Name:   "image",
		Kind:   release.ArtifactImage,
		Ref:    digest,
		Digest: digestOnly(digest),
	}, nil
}

// imageDigest reads back the repository digest assigned at push time.
func imageDigest(ctx context.Context, dir, tag string) (string, error) {
	out, err := run(ctx, dir, "docker", "inspect", "--format", "{{index .RepoDigests 0}}", tag)
	if err != nil {
		return "", fmt.Errorf("could not resolve %s to a digest: %w\n%s", tag, err, out)
	}
	digest := strings.TrimSpace(out)
	if !strings.Contains(digest, "@sha256:") {
		return "", fmt.Errorf("expected a digest reference for %s, got %q", tag, digest)
	}
	return digest, nil
}

func digestOnly(ref string) string {
	if _, d, ok := strings.Cut(ref, "@"); ok {
		return d
	}
	return ref
}

func shortCommit(commit string) string {
	if len(commit) >= 12 {
		return commit[:12]
	}
	if commit == "" {
		return "local"
	}
	return commit
}

// collect copies the declared build outputs into a staging directory, which
// becomes the release's contents.
func collect(srcDir string, outputs []string) (string, []release.Artifact, error) {
	stage, err := os.MkdirTemp("", "pilot-stage-*")
	if err != nil {
		return "", nil, err
	}

	var artifacts []release.Artifact
	for _, out := range outputs {
		from := filepath.Join(srcDir, out)
		fi, err := os.Stat(from)
		if err != nil {
			os.RemoveAll(stage)
			return "", nil, fmt.Errorf("build output %q not found: %w", out, err)
		}

		kind := release.ArtifactFile
		to := filepath.Join(stage, filepath.Base(strings.TrimSuffix(out, "/")))
		if fi.IsDir() {
			kind = release.ArtifactDir
			// A trailing slash means "the contents of", matching rsync.
			if strings.HasSuffix(out, "/") {
				to = stage
			}
		}

		if err := copyPath(from, to); err != nil {
			os.RemoveAll(stage)
			return "", nil, fmt.Errorf("staging build output %q: %w", out, err)
		}

		digest, size, err := hashPath(from)
		if err != nil {
			os.RemoveAll(stage)
			return "", nil, err
		}
		artifacts = append(artifacts, release.Artifact{
			Name: out, Kind: kind, Digest: digest, Size: size,
		})
	}
	return stage, artifacts, nil
}

func copyPath(from, to string) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		return os.WriteFile(to, data, fi.Mode().Perm())
	}

	return filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			os.Remove(dst)
			return os.Symlink(target, dst)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, info.Mode().Perm())
	})
}

// hashPath digests a file or directory by content, so an identical build
// produces an identical release hash.
func hashPath(p string) (string, int64, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", 0, err
	}
	if !fi.IsDir() {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", 0, err
		}
		return release.HashBytes(data), fi.Size(), nil
	}

	h := release.NewHasher()
	var total int64
	err = filepath.Walk(p, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(p, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h.Add(filepath.ToSlash(rel), data)
		total += info.Size()
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	return h.Sum(), total, nil
}

func run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}
