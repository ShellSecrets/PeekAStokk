// Package config loads peekastokk's optional configuration file.
//
// The file is a plain "key = value" format so it stays dependency-free:
//
//	# PeekAStokk configuration
//	port      = 9000            ; or: addr = 0.0.0.0:9000
//	history   = 5000
//	poll      = 100ms
//	log-level = debug
//
//	file = /var/log/app.log
//	file = ~/logs/worker.log    # repeat "file" for each log
//
// Blank lines and lines starting with # or ; are ignored, as is an
// unquoted trailing "  # comment". Values may be double- or single-quoted
// to preserve leading/trailing spaces or a literal #. In "file" values a
// leading ~ expands to the home directory and relative paths are resolved
// against the directory containing the config file. Every key except
// "file" may appear at most once.
//
// Unless an explicit path is given, the file is looked up in (first match
// wins):
//
//	$XDG_CONFIG_HOME/peekastokk/config   (~/.config/peekastokk/config)
//	~/.peekastokk
//	~/.peekastokk/config
//	/etc/peekastokk/config
//
// The last entry is an unconditional, environment-independent fallback —
// it is what a systemd (or other init) service account with no meaningful
// $HOME resolves to, without needing any special environment setup.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds the values parsed from a configuration file. Scalar fields
// are only meaningful when Has reports the key was present, so absence can
// be told apart from a zero value when merging with flag defaults.
type Config struct {
	Path string // the file the configuration was loaded from

	Addr         string
	Port         int
	History      int
	Lines        int
	Poll         time.Duration
	Rescan       time.Duration
	TailBytes    int64
	MaxLineBytes int
	LogLevel     string
	Auth         string // "user:password"; empty means no authentication
	Files        []string

	// Forwarding (client side): where to push tailed lines, and the
	// bearer token authenticating this client to that server.
	ForwardTo          string
	ForwardToken       string
	ForwardBufferLines int
	// Headless suppresses the local web UI entirely (a pure forwarder).
	Headless bool
	// StatusAddr, when set, serves a minimal /healthz + /statusz listener
	// (no log content) independent of Headless.
	StatusAddr string

	// Docker log discovery (client side).
	Docker           bool
	DockerRoot       string
	DockerPoll       time.Duration
	DockerContainers []string // exact[:alias], glob, or "*"; repeatable

	// Ingest (server side): client name -> bearer token or Argon2id hash.
	// Presence of any entry enables the /ingest endpoint.
	Ingest map[string]string

	present map[string]bool
}

// Has reports whether key appeared in the file. Keys match the flag names:
// "addr", "port", "history", "lines", "poll", "rescan", "tail-bytes",
// "max-line-bytes", "log-level", "auth", "file", "forward-to",
// "forward-token", "forward-buffer-lines", "headless", "status-addr",
// "docker", "docker-root", "docker-poll", "docker-containers", "ingest".
func (c *Config) Has(key string) bool { return c.present[key] }

// Load reads the configuration from explicitPath or, when it is empty, from
// the first existing file in DefaultPaths. It returns (nil, nil) when no
// path was given and no default file exists; a missing explicit path is an
// error.
func Load(explicitPath string) (*Config, error) {
	path := explicitPath
	if path == "" {
		path = firstRegularFile(DefaultPaths())
		if path == "" {
			return nil, nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c, err := parse(string(data), filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.Path = path
	return c, nil
}

// etcConfigPath is the unconditional system-wide fallback, checked last.
// A var (not a const) so tests can redirect it instead of touching the
// real /etc; production code never reassigns it.
var etcConfigPath = "/etc/peekastokk/config"

// DefaultPaths returns the candidate config file locations in search order.
func DefaultPaths() []string {
	home, _ := os.UserHomeDir()
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" && home != "" {
		xdg = filepath.Join(home, ".config")
	}

	var paths []string
	if xdg != "" {
		paths = append(paths, filepath.Join(xdg, "peekastokk", "config"))
	}
	if home != "" {
		paths = append(paths,
			filepath.Join(home, ".peekastokk"),
			filepath.Join(home, ".peekastokk", "config"))
	}
	if etcConfigPath != "" {
		paths = append(paths, etcConfigPath)
	}
	return paths
}

func firstRegularFile(paths []string) string {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

func parse(text, baseDir string) (*Config, error) {
	c := &Config{present: make(map[string]bool)}
	sc := bufio.NewScanner(strings.NewReader(text))
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		rawKey, rawValue, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected \"key = value\"", lineNo)
		}
		key := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(rawKey)), "_", "-")
		value, err := parseValue(rawValue)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if key != "file" && key != "docker-containers" && key != "ingest" && c.present[key] {
			return nil, fmt.Errorf("line %d: duplicate key %q", lineNo, key)
		}

		switch key {
		case "addr":
			if value == "" {
				err = errors.New("must not be empty")
			}
			c.Addr = value
		case "port":
			c.Port, err = parsePort(value)
		case "history":
			c.History, err = strconv.Atoi(value)
			if err == nil && c.History < 0 {
				err = errors.New("must not be negative")
			}
		case "lines":
			c.Lines, err = strconv.Atoi(value)
			if err == nil && c.Lines <= 0 {
				err = errors.New("must be positive")
			}
		case "poll":
			c.Poll, err = time.ParseDuration(value)
			if err == nil && c.Poll <= 0 {
				err = errors.New("must be positive")
			}
		case "rescan":
			c.Rescan, err = time.ParseDuration(value)
			if err == nil && c.Rescan < 0 {
				err = errors.New("must not be negative (0 disables re-scanning)")
			}
		case "tail-bytes":
			c.TailBytes, err = strconv.ParseInt(value, 10, 64)
		case "max-line-bytes":
			c.MaxLineBytes, err = strconv.Atoi(value)
			if err == nil && c.MaxLineBytes <= 0 {
				err = errors.New("must be positive")
			}
		case "log-level":
			var lvl slog.Level
			err = lvl.UnmarshalText([]byte(value))
			c.LogLevel = value
		case "auth":
			if user, pass, ok := strings.Cut(value, ":"); !ok || user == "" || pass == "" {
				err = errors.New(`must be "user:password" with both parts non-empty`)
			}
			c.Auth = value
		case "file":
			var p string
			p, err = resolveFilePath(value, baseDir)
			c.Files = append(c.Files, p)
		case "forward-to":
			if value == "" {
				err = errors.New("must not be empty")
			} else if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
				err = errors.New("must be an http:// or https:// URL")
			}
			c.ForwardTo = value
		case "forward-token":
			if value == "" {
				err = errors.New("must not be empty")
			}
			c.ForwardToken = value
		case "forward-buffer-lines":
			c.ForwardBufferLines, err = strconv.Atoi(value)
			if err == nil && c.ForwardBufferLines <= 0 {
				err = errors.New("must be positive")
			}
		case "headless":
			c.Headless, err = strconv.ParseBool(value)
		case "status-addr":
			if value == "" {
				err = errors.New("must not be empty")
			}
			c.StatusAddr = value
		case "docker":
			c.Docker, err = strconv.ParseBool(value)
		case "docker-root":
			if value == "" {
				err = errors.New("must not be empty")
			}
			c.DockerRoot = value
		case "docker-poll":
			c.DockerPoll, err = time.ParseDuration(value)
			if err == nil && c.DockerPoll <= 0 {
				err = errors.New("must be positive")
			}
		case "docker-containers":
			if value == "" {
				err = errors.New("must not be empty")
			}
			c.DockerContainers = append(c.DockerContainers, value)
		case "ingest":
			name, secret, ok := strings.Cut(value, ":")
			switch {
			case !ok || name == "" || secret == "":
				err = errors.New(`must be "name:token-or-hash" with both parts non-empty`)
			case strings.ContainsAny(name, "/ \t"):
				err = errors.New("name must not contain '/' or whitespace")
			default:
				if c.Ingest == nil {
					c.Ingest = make(map[string]string)
				}
				if _, dup := c.Ingest[name]; dup {
					err = fmt.Errorf("duplicate ingest name %q", name)
				} else {
					c.Ingest[name] = secret
				}
			}
		default:
			return nil, fmt.Errorf("line %d: unknown key %q", lineNo, key)
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: invalid %s value %q: %v", lineNo, key, value, err)
		}
		c.present[key] = true
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if c.present["addr"] && c.present["port"] {
		return nil, errors.New(`set "addr" or "port", not both`)
	}
	return c, nil
}

// parseValue trims rawValue, strips an unquoted trailing comment, and
// removes surrounding quotes.
func parseValue(rawValue string) (string, error) {
	v := strings.TrimSpace(rawValue)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if v[len(v)-1] != v[0] {
			return "", fmt.Errorf("unterminated quoted value %s", v)
		}
		return v[1 : len(v)-1], nil
	}
	// A # only starts a comment when preceded by whitespace, so values
	// like "app#1.log" survive.
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	} else if i := strings.Index(v, "\t#"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v, nil
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil {
		return 0, errors.New("not a number")
	}
	if port < 1 || port > 65535 {
		return 0, errors.New("must be between 1 and 65535")
	}
	return port, nil
}

// resolveFilePath expands a leading ~ to the home directory and resolves
// relative paths against the config file's own directory, so the meaning of
// the config does not depend on the working directory peekastokk happens to
// be started from.
func resolveFilePath(value, baseDir string) (string, error) {
	if value == "" {
		return "", errors.New("must not be empty")
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %v", err)
		}
		return filepath.Join(home, strings.TrimPrefix(value[1:], "/")), nil
	}
	if !filepath.IsAbs(value) {
		return filepath.Join(baseDir, value), nil
	}
	return value, nil
}
