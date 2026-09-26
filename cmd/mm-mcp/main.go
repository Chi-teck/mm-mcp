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

// initTimeout bounds the startup checks (GET /users/me and the team lookup).
var initTimeout = 10 * time.Second

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
// disconnects or ctx is canceled. Any failure is printed as one line on
// stderr and yields exit status 1. Nothing but MCP frames goes to stdout.
func run(ctx context.Context, args []string, lookup func(string) (string, bool), stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, "mm-mcp "+buildVersion())
		return 0
	}

	fail := func(err error) int {
		// One line even if a server message spans several.
		_, _ = fmt.Fprintln(stderr, "mm-mcp: "+strings.Join(strings.Fields(err.Error()), " "))
		return 1
	}

	cfg, err := config.Load(lookup)
	if err != nil {
		return fail(err)
	}

	mm := mattermost.NewContext(cfg)
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	err = mm.Init(initCtx)
	timedOut := errors.Is(initCtx.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil && timedOut {
		err = fmt.Errorf("no answer from Mattermost within %s", initTimeout)
	}
	if err != nil {
		return fail(err)
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
