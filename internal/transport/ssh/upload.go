package ssh

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/gandalfledev/pilot/internal/transport"
)

// UploadDir copies a local directory tree to the host.
//
// rsync is used when the host has it, because staging a static site's assets
// repeatedly is mostly unchanged bytes. Otherwise Pilot streams an in-process
// tar over the existing ssh connection — which needs no local tar binary and so
// behaves identically when the operator is on macOS.
func (c *Client) UploadDir(ctx context.Context, localDir, remoteDir string) error {
	fi, err := os.Stat(localDir)
	if err != nil {
		return fmt.Errorf("staging %s: %w", localDir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("staging %s: not a directory", localDir)
	}

	if c.canRsync(ctx) {
		return c.rsyncDir(ctx, localDir, remoteDir)
	}
	return c.tarDir(ctx, localDir, remoteDir)
}

// canRsync reports whether rsync exists on both ends. Either side missing it
// means falling back to tar, which always works.
func (c *Client) canRsync(ctx context.Context) bool {
	if _, err := exec.LookPath(c.cfg.rsyncBinary()); err != nil {
		return false
	}
	ok, err := c.HasCommand(ctx, "rsync")
	return err == nil && ok
}

func (c *Client) rsyncDir(ctx context.Context, localDir, remoteDir string) error {
	var stdout, stderr bytes.Buffer
	args := c.cfg.RsyncArgs(localDir, remoteDir)

	code, err := runCommand(ctx, c.cfg.rsyncBinary(), args, nil, &stdout, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("rsync to %s failed (exit %d): %s",
			c.Label(), code, firstLine(stderr.String()))
	}
	return nil
}

// tarDir streams the tree as a tar archive into a remote extract.
func (c *Client) tarDir(ctx context.Context, localDir, remoteDir string) error {
	pr, pw := io.Pipe()

	go func() {
		pw.CloseWithError(writeTar(localDir, pw))
	}()
	defer pr.Close()

	remote := fmt.Sprintf("mkdir -p %s && tar -C %s -xf -", transport.Quote(remoteDir), transport.Quote(remoteDir))

	var stdout, stderr bytes.Buffer
	code, err := c.exec(ctx, c.cfg.Args(c.cfg.Wrap(remote)), pr, &stdout, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("uploading to %s:%s failed (exit %d): %s",
			c.Label(), remoteDir, code, firstLine(stderr.String()))
	}
	return nil
}

// writeTar archives dir's contents (not dir itself) in lexical order.
func writeTar(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err
		}

		// Preserve symlinks rather than following them: a build output may
		// legitimately contain one, and dereferencing would silently duplicate
		// content or loop.
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Uname, hdr.Gname = "", ""
		hdr.Uid, hdr.Gid = 0, 0

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		tw.Close()
		return err
	}
	return tw.Close()
}

// MkdirAll creates a remote directory.
func (c *Client) MkdirAll(ctx context.Context, remoteDir string) error {
	res, err := c.Run(ctx, "mkdir -p "+transport.Quote(remoteDir))
	if err != nil {
		return err
	}
	return res.Err()
}

// RemoveAll deletes a remote path.
//
// The guard is not paranoia for its own sake: this runs as root on a real
// server, and a path built from an empty variable would otherwise expand to
// something catastrophic.
func (c *Client) RemoveAll(ctx context.Context, remotePath string) error {
	clean := path.Clean(remotePath)
	if clean == "" || clean == "/" || clean == "." || !strings.HasPrefix(clean, "/") {
		return fmt.Errorf("refusing to remove %q: expected an absolute path below /", remotePath)
	}
	if strings.Count(clean, "/") < 2 {
		return fmt.Errorf("refusing to remove %q: too close to the filesystem root", clean)
	}
	res, err := c.Run(ctx, "rm -rf "+transport.Quote(clean))
	if err != nil {
		return err
	}
	return res.Err()
}
