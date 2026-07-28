package secrets

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DotenvFile is the file Pilot loads from the fleet directory, if present.
//
// It exists so that `pilot deploy` is a command you can type, rather than one
// you have to remember to prefix with a variable every time. It is loaded, not
// sourced — nothing is exported to your shell.
const DotenvFile = ".env"

// LoadDotenv reads KEY=VALUE pairs from the fleet directory's .env into the
// process environment.
//
// Existing variables win. That ordering matters: it lets you override a single
// value for one run (`FOO=bar pilot deploy ...`) without editing the file, and
// it means CI, where secrets arrive as real environment variables, needs no
// .env at all.
//
// A missing file is not an error — most fleets will not have one.
func LoadDotenv(fleetRoot string) (loaded []string, err error) {
	path := filepath.Join(fleetRoot, DotenvFile)

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	if err := warnIfWorldReadable(path); err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		key, value, ok := parseDotenvLine(scanner.Text())
		if !ok {
			continue
		}
		if key == "" {
			return nil, fmt.Errorf("%s:%d: malformed line", path, line)
		}
		// Already set wins, so a one-off override on the command line works.
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return nil, err
		}
		loaded = append(loaded, key)
	}
	return loaded, scanner.Err()
}

// warnIfWorldReadable refuses a secrets file the rest of the machine can read.
//
// This is the one place Pilot is strict about a local file's mode, because the
// whole point of the file is that it holds secrets, and 0644 is the default
// almost every editor will give it.
func warnIfWorldReadable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is mode %04o, readable by other users on this machine\n"+
			"fix with:  chmod 600 %s", path, perm, path)
	}
	return nil
}

// parseDotenvLine reads one KEY=VALUE line, handling comments, `export`
// prefixes, and quoted values.
func parseDotenvLine(raw string) (key, value string, ok bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")

	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", true // signals malformed rather than skippable
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	switch {
	case len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`):
		value = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(value[1 : len(value)-1])
	case len(value) >= 2 && strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'"):
		// Single quotes are literal, as in a shell.
		value = value[1 : len(value)-1]
	default:
		// An unquoted value ends at an inline comment.
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
	}
	return key, value, true
}
