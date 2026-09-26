package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

func fakeEnv(fake *testutil.Server) map[string]string {
	return map[string]string{
		config.EnvURL:   fake.URL,
		config.EnvToken: testutil.Token,
		config.EnvTeam:  testutil.TeamName,
	}
}

// runFail runs with empty stdin, expects exit 1, no stdout, and exactly one
// stderr line starting with "mm-mcp:" and a space; it returns that line.
func runFail(t *testing.T, lookup func(string) (string, bool)) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, lookup, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	line := stderr.String()
	if !strings.HasPrefix(line, "mm-mcp: ") || strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Errorf("stderr = %q, want one line starting with \"mm-mcp: \"", line)
	}
	if strings.Contains(line, testutil.Token) {
		t.Error("stderr contains the token")
	}
	return strings.TrimSuffix(line, "\n")
}

func TestRunConfigErrorSkipsNetwork(t *testing.T) {
	fake := testutil.New(t)
	vars := fakeEnv(fake)
	delete(vars, config.EnvTeam)
	vars[config.EnvToken] = ""

	line := runFail(t, env(vars))
	for _, want := range []string{config.EnvToken, config.EnvTeam} {
		if !strings.Contains(line, want) {
			t.Errorf("stderr %q does not name %s", line, want)
		}
	}
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("made %d requests, want none after a config error", n)
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	lookup := func(string) (string, bool) {
		t.Error("--version read the environment")
		return "", false
	}
	code := run(context.Background(), []string{"--version"}, lookup, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if got, want := stdout.String(), "mm-mcp "+buildVersion()+"\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
}

func TestRunNoConfig(t *testing.T) {
	line := runFail(t, env(nil))
	for _, want := range []string{config.EnvURL, config.EnvToken, config.EnvTeam} {
		if !strings.Contains(line, want) {
			t.Errorf("stderr %q does not name %s", line, want)
		}
	}
}

func TestRunBadToken(t *testing.T) {
	fake := testutil.New(t)
	vars := fakeEnv(fake)
	vars[config.EnvToken] = "wrongtoken0000000000000000"
	line := runFail(t, env(vars))
	if !strings.Contains(line, "mattermost API 401 /api/v4/users/me") {
		t.Errorf("stderr = %q, want the 401 on /users/me", line)
	}
	if strings.Contains(line, "wrongtoken") {
		t.Error("stderr contains the token")
	}
}

func TestRunUnknownTeam(t *testing.T) {
	fake := testutil.New(t)
	vars := fakeEnv(fake)
	vars[config.EnvTeam] = "no-such-team"
	line := runFail(t, env(vars))
	if !strings.Contains(line, "no-such-team") {
		t.Errorf("stderr = %q, want it to name the team", line)
	}
}

func TestRunInitTimeout(t *testing.T) {
	old := initTimeout
	initTimeout = 50 * time.Millisecond
	t.Cleanup(func() { initTimeout = old })

	fake := testutil.New(t)
	fake.Handle(http.MethodGet, "/users/{user_id}", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	line := runFail(t, env(fakeEnv(fake)))
	if want := "mm-mcp: no answer from Mattermost within 50ms"; line != want {
		t.Errorf("stderr = %q, want %q", line, want)
	}
}

func TestRunServesStdio(t *testing.T) {
	fake := testutil.New(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vars := fakeEnv(fake)
	vars[config.EnvUploadRoot] = root
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), nil, env(vars), inR, outW, &stderr)
		_ = outW.Close()
	}()

	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n"
	if _, err := io.WriteString(inW, req); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(outR).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			ServerInfo struct{ Name, Version string } `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil || resp.ID != 1 || resp.Result.ServerInfo.Name != "mm-mcp" ||
		resp.Result.ServerInfo.Version != buildVersion() {
		t.Errorf("stdout = %q, want an initialize response from mm-mcp %s (%v)", line, buildVersion(), err)
	}

	_ = inW.Close() // client disconnects
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after stdin closed")
	}
	if want := "mm-mcp: upload root: " + root + "\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestRunMultiLineServerMessage(t *testing.T) {
	fake := testutil.New(t)
	fake.Handle(http.MethodGet, "/users/{user_id}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusInternalServerError, "x", "first line\n  second line")
	})
	line := runFail(t, env(fakeEnv(fake)))
	if !strings.HasSuffix(line, ": first line second line") {
		t.Errorf("stderr = %q, want the server message on one line", line)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	fake := testutil.New(t)
	t.Chdir("/") // as GUI hosts start servers; with MM_MCP_UPLOAD_ROOT unset, attachments are off
	inR, inW := io.Pipe()
	t.Cleanup(func() { _ = inW.Close() })
	outR, outW := io.Pipe()
	var stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, nil, env(fakeEnv(fake)), inR, outW, &stderr)
		_ = outW.Close()
	}()

	// Once initialize is answered, startup is over; then signal a stop.
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n"
	if _, err := io.WriteString(inW, req); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(outR).ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, outR) }()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if want := "mm-mcp: attachments disabled: set MM_MCP_UPLOAD_ROOT to enable them\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

// TestMainProcess runs main in a child process started by startMain.
func TestMainProcess(t *testing.T) {
	grace, ok := os.LookupEnv("MM_MCP_TEST_STOP_GRACE")
	if !ok {
		t.Skip("child process helper")
	}
	d, err := time.ParseDuration(grace)
	if err != nil {
		t.Fatal(err)
	}
	stopGrace = d
	main()
}

// startMain starts main in a child process with the given stop grace, makes a
// get_post call that hangs on the fake server and returns once it is in flight.
func startMain(t *testing.T, grace time.Duration) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	fake := testutil.New(t)
	inFlight := make(chan struct{}, 1)
	fake.Handle(http.MethodGet, "/posts/{post_id}", func(_ http.ResponseWriter, r *http.Request) {
		inFlight <- struct{}{}
		<-r.Context().Done()
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcess$")
	cmd.Env = append(os.Environ(), "MM_MCP_TEST_STOP_GRACE="+grace.String(),
		config.EnvURL+"="+fake.URL, config.EnvToken+"="+testutil.Token, config.EnvTeam+"="+testutil.TeamName,
		config.EnvUploadRoot+"="+t.TempDir())
	inW, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inW.Close() })
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	reqs := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_post","arguments":{"post_id":"` + strings.Repeat("a", 26) + `"}}}
`
	if _, err := io.WriteString(inW, reqs); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatalf("get_post never reached the server (stderr %q)", stderr.String())
	}
	return cmd, &stderr
}

// waitExit waits up to limit for cmd to exit and returns its exit error.
func waitExit(t *testing.T, cmd *exec.Cmd, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("process did not exit within %s", limit)
		return nil
	}
}

func TestMainSignalWithHangingCall(t *testing.T) {
	cmd, stderr := startMain(t, 200*time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := waitExit(t, cmd, 5*time.Second)
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 1 {
		t.Errorf("exit = %v, want status 1", err)
	}
	if want := "mm-mcp: no clean stop within 200ms\n"; !strings.HasSuffix(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to end with %q", stderr.String(), want)
	}
}

func TestMainSecondSignalKills(t *testing.T) {
	cmd, _ := startMain(t, time.Minute)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	// The first signal is handled; retry until the restored default kills the process.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-done:
			if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
				t.Errorf("exit = %v, want killed by SIGTERM", err)
			}
			return
		case <-deadline:
			t.Fatal("process survived a second SIGTERM")
		case <-time.After(50 * time.Millisecond):
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}
}
