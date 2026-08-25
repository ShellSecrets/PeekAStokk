// Command peekastokk tails one or more log files and streams them live to a
// browser over Server-Sent Events.
//
//	peekastokk /var/log/app.log /var/log/nginx/access.log
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
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

	"golang.org/x/term"

	"github.com/shellsecrets/peekastokk/internal/auth"
	"github.com/shellsecrets/peekastokk/internal/config"
	"github.com/shellsecrets/peekastokk/internal/dockerwatch"
	"github.com/shellsecrets/peekastokk/internal/forward"
	"github.com/shellsecrets/peekastokk/internal/hub"
	"github.com/shellsecrets/peekastokk/internal/server"
	"github.com/shellsecrets/peekastokk/internal/tail"
	"github.com/shellsecrets/peekastokk/internal/tailgroup"
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
		addr          = flag.String("addr", "127.0.0.1:8844", "HTTP listen address")
		historySize   = flag.Int("history", 2000, "number of recent lines replayed to newly connected browsers")
		uiLines       = flag.Int("lines", 500, "default number of lines the web UI keeps on screen (adjustable in the UI)")
		poll          = flag.Duration("poll", 200*time.Millisecond, "how often tailed files are checked for new data")
		rescan        = flag.Duration("rescan", time.Minute, "how often directory and glob arguments are re-scanned for created or deleted log files (0 disables: expand once at startup)")
		tailBytes     = flag.Int64("tail-bytes", 64*1024, "maximum bytes of existing content replayed per file at startup (negative starts at end of file)")
		maxLine       = flag.Int("max-line-bytes", 256*1024, "lines longer than this are split into chunks")
		logLevel      = flag.String("log-level", "info", "log level: debug, info, warn, or error")
		authFlag      = flag.String("auth", "", `require HTTP basic auth to view logs, format "user:password" or "user:$argon2id$..." (empty disables; default). Prefer the config file, with a hash from -hash-password`)
		hashPassword  = flag.Bool("hash-password", false, "prompt for a password, print its Argon2id hash for the auth setting, and exit")
		generateToken = flag.Bool("generate-token", false, "generate a forwarding token: print the plaintext once (for the client's forward-token) and its hash (for the server's ingest setting), then exit")
		configPath    = flag.String("config", "", "path to a config file (default: search the standard locations)")
		showVersion   = flag.Bool("version", false, "print version and exit")

		headless     = flag.Bool("headless", false, "do not serve the web UI; forward only")
		statusAddr   = flag.String("status-addr", "", "optional address for a minimal /healthz + /statusz listener (no log content)")
		forwardTo    = flag.String("forward-to", "", "push tailed lines to this PeekAStokk server (http(s)://host:port); requires -forward-token")
		forwardToken = flag.String("forward-token", "", "bearer token for -forward-to (from the server operator's -generate-token)")
		forwardBuf   = flag.Int("forward-buffer-lines", forward.DefaultBufferLines, "lines buffered in memory while the forward connection is down (oldest dropped when full)")
		dockerOn     = flag.Bool("docker", false, "tail Docker container logs found under -docker-root")
		dockerRoot   = flag.String("docker-root", dockerwatch.DefaultRoot, "Docker containers directory (docker info --format '{{.DockerRootDir}}' + /containers)")
		dockerPoll   = flag.Duration("docker-poll", dockerwatch.DefaultPoll, "how often the containers directory is re-scanned")
	)
	var dockerContainers multiFlag
	flag.Var(&dockerContainers, "docker-containers", `container to tail: exact "name[:alias]", a glob like 'web-*', or '*' for all (repeatable; default all)`)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"Usage: peekastokk [flags] [logfile|directory|glob ...]\n\n"+
				"Tails the given log files and streams them live to a web UI.\n"+
				"A directory means every file directly inside it; glob patterns\n"+
				"(e.g. '/var/log/*.log', quoted so the shell passes them through)\n"+
				"expand to their matches. Directories and patterns are re-scanned\n"+
				"every -rescan, picking up files created or deleted later.\n"+
				"An argument may end in :alias to name the source: on a file the\n"+
				"alias is the name, on a directory or glob it prefixes each file\n"+
				"(e.g. '/var/log/myapp/:worker1' -> worker1/app.log).\n"+
				"Files and flag defaults may also come from a config file, searched at:\n\n\t%s\n\nFlags:\n",
			strings.Join(config.DefaultPaths(), "\n\t"))
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("peekastokk", version)
		return nil
	}
	if *hashPassword {
		return runHashPassword()
	}
	if *generateToken {
		return runGenerateToken()
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
		if !fromCLI["rescan"] && cfg.Has("rescan") {
			*rescan = cfg.Rescan
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
		// -auth "" on the command line deliberately disables auth from the
		// config, per the usual flags-beat-config rule.
		if !fromCLI["auth"] && cfg.Has("auth") {
			*authFlag = cfg.Auth
		}
		if !fromCLI["headless"] && cfg.Has("headless") {
			*headless = cfg.Headless
		}
		if !fromCLI["status-addr"] && cfg.Has("status-addr") {
			*statusAddr = cfg.StatusAddr
		}
		if !fromCLI["forward-to"] && cfg.Has("forward-to") {
			*forwardTo = cfg.ForwardTo
		}
		if !fromCLI["forward-token"] && cfg.Has("forward-token") {
			*forwardToken = cfg.ForwardToken
		}
		if !fromCLI["forward-buffer-lines"] && cfg.Has("forward-buffer-lines") {
			*forwardBuf = cfg.ForwardBufferLines
		}
		if !fromCLI["docker"] && cfg.Has("docker") {
			*dockerOn = cfg.Docker
		}
		if !fromCLI["docker-root"] && cfg.Has("docker-root") {
			*dockerRoot = cfg.DockerRoot
		}
		if !fromCLI["docker-poll"] && cfg.Has("docker-poll") {
			*dockerPoll = cfg.DockerPoll
		}
		if !fromCLI["docker-containers"] && cfg.Has("docker-containers") {
			dockerContainers = cfg.DockerContainers
		}
	}

	if (*forwardTo == "") != (*forwardToken == "") {
		return errors.New("forward-to and forward-token must be set together")
	}

	var authUser, authPass string
	if *authFlag != "" {
		user, pass, ok := strings.Cut(*authFlag, ":")
		if !ok || user == "" || pass == "" {
			return errors.New(`-auth must be "user:password" with both parts non-empty`)
		}
		authUser, authPass = user, pass
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

	// Files on the command line replace the config file's list entirely.
	args := flag.Args()
	if len(args) == 0 && cfg != nil {
		args = cfg.Files
	}
	// A pure receiver (only ingest= entries, no local sources) is valid.
	hasIngest := cfg != nil && len(cfg.Ingest) > 0
	// With re-scanning enabled, directory and glob arguments are watched:
	// their current files are tailed and the argument is re-expanded every
	// -rescan so files created or deleted later are picked up. With it
	// disabled everything expands once at startup, as before.
	logArgs := parseLogArgs(args)
	var watchArgs []logArg
	if *rescan > 0 {
		logArgs, watchArgs = splitWatchArgs(logArgs)
	}
	var sources []logSource
	if len(logArgs) > 0 {
		sources, err = resolvePaths(logArgs, logger)
		if err != nil {
			flag.Usage()
			return err
		}
	} else if len(watchArgs) == 0 && !*dockerOn && !hasIngest {
		flag.Usage()
		return errors.New(`at least one log source is required: file arguments, "file =" config entries, docker = true, or (for a receiving server) "ingest =" entries`)
	}
	// staticIDs keeps watched scans from double-tailing a file that is
	// already tailed via a plain argument.
	staticIDs := make(map[string]bool, len(sources))
	for _, src := range sources {
		if id, idErr := pathIdentity(src.path); idErr == nil {
			staticIDs[id] = true
		}
	}
	var dynSources []logSource
	if len(watchArgs) > 0 {
		dynSources = scanWatchArgs(watchArgs, staticIDs, maxTailedFiles-len(sources), logger)
	}
	if *uiLines <= 0 {
		return fmt.Errorf("-lines must be positive, got %d", *uiLines)
	}

	// Parse the container selector up front so a bad entry fails startup.
	var dockerSel *dockerwatch.Selector
	if *dockerOn {
		if dockerSel, err = dockerwatch.NewSelector(dockerContainers); err != nil {
			return err
		}
	}
	if *headless && *forwardTo == "" && !*dockerOn && len(sources) == 0 {
		logger.Warn("headless with nothing to forward or view — probably a misconfiguration")
	}

	// Claim ports before starting anything, so a taken port is a clean
	// startup failure instead of a half-started process. A headless
	// forwarder deliberately binds no viewer port at all.
	var listener net.Listener
	if !*headless {
		if occupiedBy := portInUse(*addr); occupiedBy != "" {
			return fmt.Errorf("%s is already in use (something is accepting connections on %s); refusing to start — pick another port", *addr, occupiedBy)
		}
		listener, err = net.Listen("tcp", *addr)
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) {
				return fmt.Errorf("port check failed: %s is already in use (is another peekastokk or service running there?)", *addr)
			}
			return fmt.Errorf("cannot listen on %s: %w", *addr, err)
		}
	} else if *forwardTo == "" {
		logger.Warn("headless without forward-to: lines are tailed and discarded")
	}
	var statusListener net.Listener
	if *statusAddr != "" {
		if occupiedBy := portInUse(*statusAddr); occupiedBy != "" {
			return fmt.Errorf("status-addr %s is already in use (something is accepting connections on %s)", *statusAddr, occupiedBy)
		}
		if statusListener, err = net.Listen("tcp", *statusAddr); err != nil {
			return fmt.Errorf("cannot listen on status-addr %s: %w", *statusAddr, err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var h *hub.Hub
	if !*headless {
		h = hub.New(*historySize)
	}

	// namer maps tailed paths to the display names used when forwarding
	// (and when registering Docker containers with the local UI).
	namer := newSourceNamer()
	for _, src := range sources {
		namer.set(src.path, src.name)
	}

	var fwd *forward.Client
	if *forwardTo != "" {
		fwd = forward.New(strings.TrimRight(*forwardTo, "/"), *forwardToken, forward.Options{
			BufferLines: *forwardBuf,
			// The reverse lookup answers the server's remote scrollback
			// requests from this host's own disk; only sources this
			// process registered ever resolve.
			ResolvePath: namer.pathOf,
			Logger:      logger,
		})
		go fwd.Run(ctx)
	}

	// All tailers fan into one channel; a single goroutine publishes to the
	// hub (and/or forwarder) so sequence numbers reflect arrival order.
	lines := make(chan tail.Line, lineBuffer)
	tailOpts := tail.Options{
		PollInterval: *poll,
		TailBytes:    *tailBytes,
		MaxLineBytes: *maxLine,
		Logger:       logger,
	}
	var tailers sync.WaitGroup
	for _, src := range sources {
		t := tail.New(src.path, tailOpts)
		tailers.Add(1)
		go func() {
			defer tailers.Done()
			t.Run(ctx, lines)
		}()
	}

	var srv *server.Server
	if !*headless {
		var ingestTokens map[string]string
		if cfg != nil {
			ingestTokens = cfg.Ingest
		}
		all := append(append([]logSource(nil), sources...), dynSources...)
		names := make(map[string]string, len(all))
		for _, src := range all {
			names[src.path] = src.name
		}
		srv = server.New(h, server.Options{
			Files:        sourcePaths(all),
			Names:        names,
			Lines:        *uiLines,
			AuthUser:     authUser,
			AuthPass:     authPass,
			IngestTokens: ingestTokens,
			Logger:       logger,
		})
	}

	// The Docker watcher re-scans the containers directory and reconciles
	// a dynamic tailer group; it joins the same WaitGroup as the static
	// tailers so shutdown ordering is unchanged.
	if *dockerOn {
		watcher := dockerwatch.NewWatcher(*dockerRoot, dockerSel, logger)
		group := tailgroup.NewGroup(ctx, lines, tailOpts)
		prev := make(map[string]bool)
		tailers.Add(1)
		go func() {
			defer tailers.Done()
			dockerwatch.Run(ctx, watcher, *dockerPoll, func(cs []dockerwatch.Container) {
				desired := make([]string, 0, len(cs))
				current := make(map[string]bool, len(cs))
				for _, c := range cs {
					desired = append(desired, c.LogPath)
					current[c.LogPath] = true
					namer.set(c.LogPath, c.DisplayName)
					if srv != nil {
						srv.RegisterSource(c.LogPath, c.DisplayName, true)
					}
				}
				for p := range prev {
					if !current[p] {
						namer.delete(p)
					}
				}
				prev = current
				group.Reconcile(desired)
			})
			group.Stop()
		}()
		logger.Info("docker log discovery enabled", "root", *dockerRoot, "poll", *dockerPoll)
	}

	// Watched directory/glob arguments mirror the Docker watcher: re-scan
	// on a ticker and reconcile a dynamic tailer group. A deleted file's
	// tailer stops and its UI entry is hidden; the same file reappearing
	// (log rotation, redeploys) revives both.
	if len(watchArgs) > 0 {
		group := tailgroup.NewGroup(ctx, lines, tailOpts)
		prev := make(map[string]bool)
		reconcile := func(found []logSource) {
			current := make(map[string]bool, len(found))
			paths := make([]string, 0, len(found))
			for _, src := range found {
				current[src.path] = true
				paths = append(paths, src.path)
				namer.set(src.path, src.name)
				if srv != nil {
					srv.RegisterSource(src.path, src.name, true)
				}
			}
			for p := range prev {
				if !current[p] {
					namer.delete(p)
					if srv != nil {
						srv.UnregisterSource(p)
					}
				}
			}
			prev = current
			group.Reconcile(paths)
		}
		limit := maxTailedFiles - len(sources)
		tailers.Add(1)
		go func() {
			defer tailers.Done()
			defer group.Stop()
			reconcile(dynSources)
			ticker := time.NewTicker(*rescan)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					reconcile(scanWatchArgs(watchArgs, staticIDs, limit, logger))
				}
			}
		}()
		logger.Info("watching for log file changes", "args", argSpecs(watchArgs), "rescan", *rescan)
	}

	publisherDone := make(chan struct{})
	go func() {
		defer close(publisherDone)
		for ln := range lines {
			if h != nil {
				h.Publish(ln.File, ln.Text, ln.Offset, ln.Time)
			}
			if fwd != nil {
				fwd.Enqueue(namer.name(ln.File), ln.Text, ln.Offset, ln.Time)
			}
		}
	}()

	serveErr := make(chan error, 2)
	var httpSrv *http.Server
	if !*headless {
		if authUser != "" {
			logger.Info("basic authentication enabled", "user", authUser)
			if !auth.IsHash(authPass) {
				logger.Warn("auth password is stored in plaintext; generate a hash with: peekastokk -hash-password")
			}
		}
		httpSrv = &http.Server{
			Addr:              *addr,
			Handler:           srv.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       120 * time.Second,
			// Read/WriteTimeout are deliberately unset: /events streams
			// responses and /ingest streams request bodies indefinitely.
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		go func() {
			serveErr <- httpSrv.Serve(listener)
		}()
		logger.Info("peekastokk listening",
			"version", version,
			"url", fmt.Sprintf("http://%s/", listener.Addr()),
			"files", sourcePaths(sources))
	} else {
		logger.Info("peekastokk headless",
			"version", version,
			"forward_to", *forwardTo,
			"files", sourcePaths(sources),
			"docker", *dockerOn)
	}
	var statusSrv *http.Server
	if statusListener != nil {
		statusSrv = &http.Server{
			Handler:           newStatusHandler(logger, fwd),
			ReadHeaderTimeout: 5 * time.Second,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		go func() {
			if err := statusSrv.Serve(statusListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Warn("status server stopped", "error", err)
			}
		}()
		logger.Info("status listener up", "addr", statusListener.Addr())
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	}

	// Close the hub first: it closes every subscriber channel, which unblocks
	// the SSE handlers so Shutdown does not have to wait out its deadline.
	if h != nil {
		h.Close()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if httpSrv != nil {
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown incomplete, forcing close", "error", err)
			httpSrv.Close()
		}
	}
	if statusSrv != nil {
		statusSrv.Close()
	}

	// Tailers (static and Docker-discovered) stop via ctx; then the
	// publisher drains and exits.
	tailers.Wait()
	close(lines)
	<-publisherDone
	return nil
}

// multiFlag is a repeatable string flag (the stdlib flag package has no
// built-in for this).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// runHashPassword prompts for a password (hidden when stdin is a terminal,
// read as one line when piped) and prints its Argon2id hash, ready for the
// "auth" setting.
func runHashPassword() error {
	readOne := func(prompt string) (string, error) {
		fd := int(os.Stdin.Fd())
		if term.IsTerminal(fd) {
			fmt.Fprint(os.Stderr, prompt)
			b, err := term.ReadPassword(fd)
			fmt.Fprintln(os.Stderr)
			return string(b), err
		}
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	pw, err := readOne("Password: ")
	if err != nil {
		return fmt.Errorf("reading password: %w", err)
	}
	if pw == "" {
		return errors.New("password must not be empty")
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		again, err := readOne("Repeat password: ")
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		if pw != again {
			return errors.New("passwords do not match")
		}
	}

	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(hash)
	fmt.Fprintf(os.Stderr, "\nPut this in your config file:\n\n\tauth = youruser:%s\n", hash)
	return nil
}

// runGenerateToken creates a high-entropy forwarding token and prints the
// plaintext (for the client) plus its Argon2id hash (for the server's
// "ingest = name:hash" line).
func runGenerateToken() error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generating token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash, err := auth.HashPassword(token)
	if err != nil {
		return err
	}
	fmt.Println(token)
	fmt.Fprintf(os.Stderr, `
Give the first line to the forwarding client (shown only once):

	forward-token = %s

Add this to the receiving server's config, choosing a name for the client:

	ingest = clientname:%s
`, token, hash)
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
// paths as expansion produced them. Every dropped duplicate is logged at
// info level so overlapping arguments (the same file via a directory and
// explicitly, overlapping globs, symlink aliases) are visible rather than
// silent.
func resolvePaths(args []logArg, logger *slog.Logger) ([]logSource, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(args) == 0 {
		return nil, errors.New(`at least one log file, directory, or glob pattern is required (as arguments or "file =" entries in a config file)`)
	}
	seen := make(map[string]string, len(args)) // identity -> path as first added
	var sources []logSource
	for _, arg := range args {
		expanded, err := expandArg(arg)
		if err != nil {
			return nil, err
		}
		added := 0
		for _, src := range expanded {
			id, err := pathIdentity(src.path)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", src.path, err)
			}
			if first, dup := seen[id]; dup {
				if first == src.path {
					logger.Info("skipping duplicate log file", "file", src.path)
				} else {
					logger.Info("skipping duplicate log file", "file", src.path, "already_tailed_as", first)
				}
				continue
			}
			seen[id] = src.path
			sources = append(sources, src)
			added++
		}
		if added == 0 && len(expanded) > 1 {
			logger.Info("argument only named files that are already tailed", "arg", arg.spec)
		}
	}
	if len(sources) > maxTailedFiles {
		return nil, fmt.Errorf("arguments expand to %d files, more than the maximum of %d", len(sources), maxTailedFiles)
	}
	return sources, nil
}

// splitWatchArgs separates arguments naming a changing set of files —
// directories and glob patterns — from plain file paths. Only used when
// re-scanning is enabled.
func splitWatchArgs(args []logArg) (plain, watch []logArg) {
	for _, a := range args {
		if strings.ContainsAny(a.spec, "*?[") {
			watch = append(watch, a)
			continue
		}
		if info, err := os.Stat(a.spec); err == nil && info.IsDir() {
			watch = append(watch, a)
			continue
		}
		plain = append(plain, a)
	}
	return plain, watch
}

// scanWatchArgs expands the watched (directory and glob) arguments into
// the log files that exist right now, skipping files already tailed via
// plain arguments and duplicates between watched arguments. Unlike the
// startup resolution, an argument currently yielding nothing is normal
// here — files coming and going is the whole point — so it is logged at
// debug rather than treated as an error.
func scanWatchArgs(watchArgs []logArg, exclude map[string]bool, limit int, logger *slog.Logger) []logSource {
	seen := make(map[string]bool)
	var sources []logSource
	for _, arg := range watchArgs {
		expanded, err := expandArg(arg)
		if err != nil {
			logger.Debug("watched argument yields no files", "arg", arg.spec, "reason", err)
			continue
		}
		for _, src := range expanded {
			id, err := pathIdentity(src.path)
			if err != nil {
				logger.Debug("skipping unresolvable path", "file", src.path, "error", err)
				continue
			}
			if exclude[id] || seen[id] {
				continue
			}
			seen[id] = true
			sources = append(sources, src)
		}
	}
	if len(sources) > limit {
		logger.Warn("watched arguments yield more files than the limit; ignoring the extra ones",
			"found", len(sources), "limit", limit)
		sources = sources[:limit]
	}
	return sources
}

// pathIdentity returns a canonical form of p for duplicate detection:
// absolute, with symlinks resolved when the file exists, so a symlink and
// its target are recognized as the same log.
func pathIdentity(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil // not created yet: the literal path is its identity
}

// logArg is one log-source entry as written on the command line or in a
// "file =" config line: a path, directory, or glob pattern, plus the
// optional display alias given as "<arg>:<alias>".
type logArg struct {
	spec  string
	alias string
}

// logSource is one concrete log file: the path tailed, and the display
// name it appears under in the UI and in forwarded lines.
type logSource struct {
	path string
	name string
}

// parseLogArgs splits the optional ":alias" suffix off each argument.
func parseLogArgs(args []string) []logArg {
	out := make([]logArg, len(args))
	for i, a := range args {
		spec, alias := config.SplitAlias(a)
		out[i] = logArg{spec: spec, alias: alias}
	}
	return out
}

// argSpecs returns just the path/pattern of each argument, for logging.
func argSpecs(args []logArg) []string {
	specs := make([]string, len(args))
	for i, a := range args {
		specs[i] = a.spec
	}
	return specs
}

// sourcePaths returns just the paths of srcs, for the callers that only
// tail or log them.
func sourcePaths(srcs []logSource) []string {
	paths := make([]string, len(srcs))
	for i, src := range srcs {
		paths[i] = src.path
	}
	return paths
}

// sourceName is the display name for path under alias. On a plain file the
// alias replaces the base name outright; on a directory or glob — which
// expand to many files — it prefixes each base name instead, so the files
// stay distinct (e.g. "worker1/app.log").
func sourceName(alias, path string, expanded bool) string {
	base := filepath.Base(path)
	switch {
	case alias == "":
		return base
	case expanded:
		return alias + "/" + base
	default:
		return alias
	}
}

// expandArg turns one CLI/config entry into concrete log sources:
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
func expandArg(arg logArg) ([]logSource, error) {
	if strings.ContainsAny(arg.spec, "*?[") {
		matches, err := filepath.Glob(arg.spec)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid pattern: %w", arg.spec, err)
		}
		var files []logSource
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() {
				files = append(files, logSource{m, sourceName(arg.alias, m, true)})
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("%s: no files match the pattern", arg.spec)
		}
		return files, nil
	}

	if info, err := os.Stat(arg.spec); err == nil && info.IsDir() {
		entries, err := os.ReadDir(arg.spec)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", arg.spec, err)
		}
		var files []logSource
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := filepath.Join(arg.spec, e.Name())
			// Stat (not e.Type) so symlinks to regular files count too.
			if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
				files = append(files, logSource{p, sourceName(arg.alias, p, true)})
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("%s: directory contains no files", arg.spec)
		}
		return files, nil
	}

	return []logSource{{arg.spec, sourceName(arg.alias, arg.spec, false)}}, nil
}
