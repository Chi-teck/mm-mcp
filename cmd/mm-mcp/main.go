// Command mm-mcp is an MCP server (stdio) that exposes Mattermost to MCP
// clients through a personal access token.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/mattermost"
	"github.com/Chi-teck/mm-mcp/internal/tools"
)

// initTimeout bounds the startup check (GET /users/me and the team lookup). It
// stays below the 10 s that clients such as Pi wait for a server to start, so
// initialize is always answered in time.
var initTimeout = 5 * time.Second

// stopGrace bounds how long run may take to return after SIGINT/SIGTERM.
// Tool handlers do not see the canceled context (go-sdk detaches them) and
// the HTTP clients have no timeout, so a hanging call would block forever.
var stopGrace = 5 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // restore default handling: a second signal kills the process
		time.Sleep(stopGrace)
		_, _ = fmt.Fprintf(os.Stderr, "mm-mcp: no clean stop within %s\n", stopGrace)
		os.Exit(1)
	}()
	code := run(ctx, os.Args[1:], os.LookupEnv, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run answers --version (on stdout, exit 0, no environment needed); otherwise
// it validates the configuration, checks the token and team within
// initTimeout, and then serves MCP on stdin/stdout until the client
// disconnects or ctx is canceled. If Mattermost is unavailable at startup
// (see mattermost.Unavailable), it prints a warning and serves anyway: each
// tool call retries the check until it succeeds. Any other failure is printed
// as one line on stderr and yields exit status 1. Nothing but MCP frames goes
// to stdout.
func run(ctx context.Context, args []string, lookup func(string) (string, bool), stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, "mm-mcp "+buildVersion())
		return 0
	}

	// One line even if a server message spans several.
	oneLine := func(err error) string { return strings.Join(strings.Fields(err.Error()), " ") }
	fail := func(err error) int {
		_, _ = fmt.Fprintln(stderr, "mm-mcp: "+oneLine(err))
		return 1
	}

	cfg, err := config.Load(lookup)
	if err != nil {
		return fail(err)
	}

	mm := mattermost.NewContext(cfg)
	if err := mm.Ready(ctx, initTimeout); err != nil {
		if ctx.Err() != nil || !mattermost.Unavailable(err) {
			return fail(err)
		}
		_, _ = fmt.Fprintln(stderr, "mm-mcp: Mattermost not reachable yet, serving anyway: "+oneLine(err))
	}

	if cfg.UploadRoot != "" {
		_, _ = fmt.Fprintln(stderr, "mm-mcp: upload root: "+cfg.UploadRoot)
	} else {
		_, _ = fmt.Fprintln(stderr, "mm-mcp: attachments disabled: set "+config.EnvUploadRoot+" to enable them")
	}

	ver := buildVersion()
	server := mcp.NewServer(&mcp.Implementation{Name: "mm-mcp", Version: ver}, nil)
	tools.Register(server, mm, ver)
	transport := &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}}
	if err := server.Run(ctx, transport); err != nil && !isShutdown(ctx, err) {
		return fail(err)
	}
	return 0
}

// isShutdown reports whether err from Server.Run is a normal end of session:
// the client closed stdin, or the process was asked to stop.
func isShutdown(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, mcp.ErrConnectionClosed)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
