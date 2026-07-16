// Command peekastokk tails one or more log files and streams them live to a
// browser over Server-Sent Events.
//
//	peekastokk /var/log/app.log /var/log/nginx/access.log
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shellsecrets/peekastokk/internal/config"
	"github.com/shellsecrets/peekastokk/internal/hub"
	"github.com/shellsecrets/peekastokk/internal/server"
	"github.com/shellsecrets/peekastokk/internal/tail"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

const (
	lineBuffer      = 1024
	shutdownTimeout = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "peekastokk:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr        = flag.String("addr", "127.0.0.1:8844", "HTTP listen address")
		historySize = flag.Int("history", 2000, "number of recent lines replayed to newly connected browsers")
		uiLines     = flag.Int("lines", 500, "default number of lines the web UI keeps on screen (adjustable in the UI)")
		poll        = flag.Duration("poll", 200*time.Millisecond, "how often tailed files are checked for new data")
		tailBytes   = flag.Int64("tail-bytes", 64*1024, "maximum bytes of existing content replayed per file at startup (negative starts at end of file)")
		maxLine     = flag.Int("max-line-bytes", 256*1024, "lines longer than this are split into chunks")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, or error")
		configPath  = flag.String("config", "", "path to a config file (default: search the standard locations)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"Usage: peekastokk [flags] [logfile|directory|glob ...]\n\n"+
				"Tails the given log files and streams them live to a web UI.\n"+
				"A directory means every file directly inside it; glob patterns\n"+
				"(e.g. '/var/log/*.log', quoted so the shell passes them through)\n"+
				"expand at startup.\n"+
				"Files and flag defaults may also come from a config file, searched at:\n\n\t%s\n\nFlags:\n",
			strings.Join(config.DefaultPaths(), "\n\t"))
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("peekastokk", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Precedence: flags given on the command line beat the config file,
	// which beats built-in defaults. flag.Visit only sees flags that were
	// explicitly set.
	fromCLI := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { fromCLI[f.Name] = true })
	if cfg != nil {
		if !fromCLI["addr"] {
			switch {
			case cfg.Has("addr"):
				*addr = cfg.Addr
			case cfg.Has("port"):
				// "port" keeps the default host and swaps the port.
				host, _, splitErr := net.SplitHostPort(*addr)
				if splitErr != nil {
					host = "127.0.0.1"
				}
				*addr = net.JoinHostPort(host, strconv.Itoa(cfg.Port))
			}
		}
		if !fromCLI["history"] && cfg.Has("history") {
			*historySize = cfg.History
		}
		if !fromCLI["lines"] && cfg.Has("lines") {
			*uiLines = cfg.Lines
		}
		if !fromCLI["poll"] && cfg.Has("poll") {
			*poll = cfg.Poll
		}
		if !fromCLI["tail-bytes"] && cfg.Has("tail-bytes") {
			*tailBytes = cfg.TailBytes
		}
		if !fromCLI["max-line-bytes"] && cfg.Has("max-line-bytes") {
			*maxLine = cfg.MaxLineBytes
		}
		if !fromCLI["log-level"] && cfg.Has("log-level") {
			*logLevel = cfg.LogLevel
		}
	}

	// Files on the command line replace the config file's list entirely.
	args := flag.Args()
	if len(args) == 0 && cfg != nil {
		args = cfg.Files
	}
	paths, err := resolvePaths(args)
	if err != nil {
		flag.Usage()
		return err
	}
	if *uiLines <= 0 {
		return fmt.Errorf("-lines must be positive, got %d", *uiLines)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid -log-level %q", *logLevel)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	if cfg != nil {
		logger.Info("loaded config", "path", cfg.Path)
	}

	// Claim the port before starting anything, so a taken port is a clean
	// startup failure instead of a half-started process.
	if occupiedBy := portInUse(*addr); occupiedBy != "" {
		return fmt.Errorf("%s is already in use (something is accepting connections on %s); refusing to start — pick another port", *addr, occupiedBy)
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("port check failed: %s is already in use (is another peekastokk or service running there?)", *addr)
		}
		return fmt.Errorf("cannot listen on %s: %w", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h := hub.New(*historySize)

	// All tailers fan into one channel; a single goroutine publishes to the
	// hub so sequence numbers reflect global arrival order.
	lines := make(chan tail.Line, lineBuffer)
	var tailers sync.WaitGroup
	for _, p := range paths {
		t := tail.New(p, tail.Options{
			PollInterval: *poll,
			TailBytes:    *tailBytes,
			MaxLineBytes: *maxLine,
			Logger:       logger,
		})
		tailers.Add(1)
		go func() {
			defer tailers.Done()
			t.Run(ctx, lines)
		}()
	}

	publisherDone := make(chan struct{})
	go func() {
		defer close(publisherDone)
		for ln := range lines {
			h.Publish(ln.File, ln.Text, ln.Offset, ln.Time)
		}
	}()

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           server.New(h, paths, *uiLines, logger).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is deliberately unset: /events streams indefinitely.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpSrv.Serve(listener)
	}()

	logger.Info("peekastokk listening",
		"version", version,
		"url", fmt.Sprintf("http://%s/", listener.Addr()),
		"files", paths)

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	}

	// Close the hub first: it closes every subscriber channel, which unblocks
	// the SSE handlers so Shutdown does not have to wait out its deadline.
	h.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown incomplete, forcing close", "error", err)
		httpSrv.Close()
	}

	// Tailers stop via ctx; then the publisher drains and exits.
	tailers.Wait()
	close(lines)
	<-publisherDone
	return nil
}

// portInUse reports whether something already accepts connections on the
// given listen address, returning the probed address that answered (empty
// when the port looks free).
//
// Relying on net.Listen failing is not enough: Go listeners set
// SO_REUSEADDR, so on macOS/BSD binding a specific address like the
// 127.0.0.1 default can quietly succeed while another process (say a node
// app on 0.0.0.0:3000) holds the wildcard address of the same port — and
// the more specific socket then shadows that app for local traffic. An
// actual connect attempt catches the wildcard listener too.
func portInUse(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "" // let net.Listen report the malformed address
	}
	probes := []string{addr}
	if host == "" || host == "0.0.0.0" || host == "::" {
		// We would bind the wildcard: a listener on any loopback address
		// (the reachable, testable ones) means a conflict.
		probes = []string{
			net.JoinHostPort("127.0.0.1", port),
			net.JoinHostPort("::1", port),
		}
	}
	for _, probe := range probes {
		conn, err := net.DialTimeout("tcp", probe, 250*time.Millisecond)
		if err == nil {
			conn.Close()
			return probe
		}
	}
	return ""
}

// maxTailedFiles caps how many files one invocation may tail, so a glob or
// directory that explodes (node_modules, /var/log on a busy box) fails
// loudly instead of spawning thousands of tailers.
const maxTailedFiles = 1000

// resolvePaths expands each argument (plain file, directory, or glob
// pattern) into concrete file paths and drops duplicates, preserving the
// paths as expansion produced them.
func resolvePaths(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, errors.New(`at least one log file, directory, or glob pattern is required (as arguments or "file =" entries in a config file)`)
	}
	seen := make(map[string]bool, len(args))
	var paths []string
	for _, arg := range args {
		expanded, err := expandArg(arg)
		if err != nil {
			return nil, err
		}
		for _, p := range expanded {
			abs, err := filepath.Abs(p)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			if seen[abs] {
				continue
			}
			seen[abs] = true
			paths = append(paths, p)
		}
	}
	if len(paths) > maxTailedFiles {
		return nil, fmt.Errorf("arguments expand to %d files, more than the maximum of %d", len(paths), maxTailedFiles)
	}
	return paths, nil
}

// expandArg turns one CLI/config entry into concrete file paths:
//
//   - a directory expands to the regular files directly inside it
//     (dot-files and subdirectories are skipped, non-recursively)
//   - a glob pattern (*, ?, [) expands to its matching regular files
//   - a plain path is kept as-is, even if it does not exist yet, so it can
//     be tailed once created
//
// Expansion happens once at startup; files created later only appear via a
// plain path, not via patterns. A pattern or directory that yields no files
// is an error rather than a silently empty viewer.
func expandArg(arg string) ([]string, error) {
	if strings.ContainsAny(arg, "*?[") {
		matches, err := filepath.Glob(arg)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid pattern: %w", arg, err)
		}
		var files []string
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() {
				files = append(files, m)
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("%s: no files match the pattern", arg)
		}
		return files, nil
	}

	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		entries, err := os.ReadDir(arg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", arg, err)
		}
		var files []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := filepath.Join(arg, e.Name())
			// Stat (not e.Type) so symlinks to regular files count too.
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
				files = append(files, p)
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("%s: directory contains no files", arg)
		}
		return files, nil
	}

	return []string{arg}, nil
}
