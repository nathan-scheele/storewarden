// Command storewarden joins a tailnet and serves a fixed set of Shopify
// Admin operations as MCP Tools, authorized by Grants in the tailnet policy
// file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tailscale.com/feature/buildfeatures"
	"tailscale.com/tsnet"

	"github.com/nathan-scheele/storewarden/internal/audit"
	"github.com/nathan-scheele/storewarden/internal/shopify"
)

const version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("storewarden stopped", "error", err)
		os.Exit(1)
	}
}

// run starts storewarden with the command-line arguments args, and returns
// when it stops. A bad flag exits the process, as flag.Parse would.
func run(args []string) error {
	// A FlagSet of its own, rather than the package's global one, so that
	// a test can call run more than once.
	flags := flag.NewFlagSet("storewarden", flag.ExitOnError)
	var (
		store       = flags.String("store", "", "the Store's `domain`, <name>.myshopify.com (required)")
		hostname    = flags.String("hostname", "storewarden", "tailnet node `name`")
		stateDir    = flags.String("state-dir", defaultStateDir(), "`directory` for the tailnet node's state")
		tags        = flags.String("tags", "tag:storewarden", "comma-separated `tags` the node advertises")
		allowWrites = flags.Bool("allow-writes", false, "Write mode: serve the Write tool to Callers granted it")
		quotaShare  = flags.Float64("quota-share", 0.5, "each Caller's Quota, as a `share` of the Store's rate limit: above 0, at most 1")
		auditPath   = flags.String("audit-log", "-", "`file` to append Audit records to, or - for stdout")
		debug       = flags.Bool("debug", false, "log at debug level, including tsnet's backend logs")
	)
	flags.Parse(args) // ExitOnError: it never returns an error

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger) // also sends the standard log package's output to logger

	// Built in, tsnet's peer API listens on every host interface
	// (docs/adr/0003-build-without-the-peer-api-server.md).
	if buildfeatures.HasPeerAPIServer {
		return errors.New("storewarden must be built with -tags ts_omit_peerapiserver")
	}
	if !isStoreDomain(*store) {
		return fmt.Errorf("--store must be <name>.myshopify.com, got %q", *store)
	}
	if *stateDir == "" {
		return errors.New("--state-dir is required")
	}
	clientID, clientSecret := os.Getenv("SHOPIFY_CLIENT_ID"), os.Getenv("SHOPIFY_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return errors.New("SHOPIFY_CLIENT_ID and SHOPIFY_CLIENT_SECRET must be set")
	}
	client, err := shopify.New("https://"+*store, clientID, clientSecret, *quotaShare, &http.Client{Timeout: 30 * time.Second}, logger)
	if err != nil {
		return fmt.Errorf("--quota-share: %w", err)
	}

	// Opened before the tailnet node starts, so the deferred Close runs
	// last: after the HTTP server and the node have shut down.
	auditLog, err := audit.Open(*auditPath, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := auditLog.Close(); err != nil {
			logger.Error("closing audit log", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// tsnet reads its own auth (TS_AUTHKEY or TS_CLIENT_SECRET) from the
	// environment. The node is persistent: Ephemeral stays false.
	ts := &tsnet.Server{
		Hostname:      *hostname,
		Dir:           *stateDir,
		AdvertiseTags: strings.Split(*tags, ","),
		UserLogf: func(format string, args ...any) {
			logger.Info(fmt.Sprintf(format, args...), "source", "tsnet")
		},
	}
	if *debug {
		ts.Logf = func(format string, args ...any) {
			logger.Debug(fmt.Sprintf(format, args...), "source", "tsnet")
		}
	}
	if err := ts.Start(); err != nil {
		return fmt.Errorf("starting tailnet node: %w", err)
	}
	defer func() {
		if err := ts.Close(); err != nil {
			logger.Warn("closing tailnet node", "error", err)
		}
	}()
	status, err := ts.Up(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopped by a signal before the node was up
		}
		return fmt.Errorf("joining tailnet: %w", err)
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return err
	}
	// The tsnet listener is the only one: storewarden is never reachable
	// from outside the tailnet.
	ln, err := ts.Listen("tcp", ":80")
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           newHandler(lc, client, *allowWrites, auditLog, logger),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	tailnet := ""
	if status.CurrentTailnet != nil {
		tailnet = status.CurrentTailnet.Name
	}
	logger.Info("serving MCP", "store", *store, "hostname", *hostname, "tailnet", tailnet, "tailscale_ips", status.TailscaleIPs, "write_mode", *allowWrites)

	select {
	case err := <-serveErr:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP server shutdown", "error", err)
	}
	return nil // the deferred ts.Close runs next, then the audit log's Close
}

func defaultStateDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "storewarden")
}

// isStoreDomain reports whether s looks like <name>.myshopify.com, so the
// client secret is only ever sent to Shopify.
func isStoreDomain(s string) bool {
	name, ok := strings.CutSuffix(s, ".myshopify.com")
	return ok && name != "" && !strings.ContainsAny(name, "./:@?#")
}
