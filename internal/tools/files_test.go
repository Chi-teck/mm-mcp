package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/mattermost"
	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

const filePattern = "/files/{file_id}"

var testFileID = postID("file")

// newDownloadHarness is newHarness with MM_MCP_DOWNLOAD_DIR set to dir.
func newDownloadHarness(t *testing.T, dir string) *harness {
	t.Helper()
	return newConfigHarness(t, config.Config{DownloadDir: dir})
}

// serveFile answers the download route with body and the given Content-Disposition ("" = none).
func serveFile(h *harness, disposition, body string) {
	h.fake.Handle(http.MethodGet, filePattern, func(w http.ResponseWriter, _ *http.Request) {
		if disposition != "" {
			w.Header().Set("Content-Disposition", disposition)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, body)
	})
}

// dirEntries lists the names in dir, sorted.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// wantSaved checks the tool output and that path holds body.
func wantSaved(t *testing.T, got, path, body string) {
	t.Helper()
	if want := "Saved " + path + " (file id: " + testFileID + ")"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	data, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatalf("file holds %q, want %q", data, body)
	}
}

func TestGetFileNames(t *testing.T) {
	cases := []struct {
		name, arg, disposition, want string
	}{
		{"plain disposition", "", `attachment; filename="report.pdf"`, "report.pdf"},
		{"rfc 5987", "", `attachment; filename="x.txt"; filename*=UTF-8''%D0%BE%D1%82%D1%87%D1%91%D1%82.txt`, "отчёт.txt"},
		{"name wins", "mine.txt", `attachment; filename="report.pdf"`, "mine.txt"},
		{"no name", "", "", testFileID + ".bin"},
		{"bad disposition", "", `attachment; filename="unterminated`, testFileID + ".bin"},
		{"separators", `../../etc/pass\wd`, "", `.._.._etc_pass_wd`},
		{"header separators", "", `attachment; filename="../evil.sh"`, ".._evil.sh"},
		{"dot dot", "..", "", testFileID + ".bin"},
		{"dot", ".", "", testFileID + ".bin"},
		{"control chars", "a\x00b\nc\x7f.txt", "", "a_b_c_.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			h := newDownloadHarness(t, dir)
			serveFile(h, tc.disposition, "payload")
			args := map[string]any{"file_id": testFileID}
			if tc.arg != "" {
				args["name"] = tc.arg
			}
			got := h.callOK(t, "get_file", args)
			wantSaved(t, got, filepath.Join(dir, tc.want), "payload")
			if names := dirEntries(t, dir); !slices.Equal(names, []string{tc.want}) {
				t.Fatalf("dir holds %q", names)
			}
			if r := h.fake.Requests(); len(r) != 1 || r[0].Path != testutil.APIPrefix+"/files/"+testFileID {
				t.Fatalf("requests %+v", r)
			}
		})
	}
}

// serverDisposition builds Content-Disposition exactly as Mattermost does
// (platform/shared/web/files.go): both forms carry url.PathEscape(name).
func serverDisposition(name string) string {
	f := url.PathEscape(name)
	return fmt.Sprintf("attachment;filename=\"%s\"; filename*=UTF-8''%s", f, f)
}

func TestGetFileServerDisposition(t *testing.T) {
	cases := map[string]string{
		"report.pdf":      "report.pdf",
		"logo@2x.png":     "logo@2x.png",
		"notes 10:30.txt": "notes 10:30.txt",
		"x=y.pdf":         "x=y.pdf",
		"a/b@c.txt":       "a_b@c.txt",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			h := newDownloadHarness(t, dir)
			serveFile(h, serverDisposition(name), "payload")
			got := h.callOK(t, "get_file", map[string]any{"file_id": testFileID})
			wantSaved(t, got, filepath.Join(dir, want), "payload")
		})
	}
}

func TestGetFileCollisions(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	serveFile(h, `attachment; filename="notes.txt"`, "new")
	for _, n := range []string{"notes.txt", "notes-1.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := h.callOK(t, "get_file", map[string]any{"file_id": testFileID})
	wantSaved(t, got, filepath.Join(dir, "notes-2.txt"), "new")
	for _, n := range []string{"notes.txt", "notes-1.txt"} {
		if data, _ := os.ReadFile(filepath.Join(dir, n)); string(data) != "old" { //nolint:gosec // test path
			t.Fatalf("%s overwritten: %q", n, data)
		}
	}
	got = h.callOK(t, "get_file", map[string]any{"file_id": testFileID})
	wantSaved(t, got, filepath.Join(dir, "notes-3.txt"), "new")
}

func TestGetFileCollisionDotfile(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	serveFile(h, "", "rc")
	if err := os.WriteFile(filepath.Join(dir, ".bashrc"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.callOK(t, "get_file", map[string]any{"file_id": testFileID, "name": ".bashrc"})
	wantSaved(t, got, filepath.Join(dir, ".bashrc-1"), "rc")
}

func TestGetFileCollisionRandomSuffix(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	serveFile(h, "", "body")
	taken := []string{"x.txt"}
	for i := 1; i < 100; i++ {
		taken = append(taken, "x-"+strconv.Itoa(i)+".txt")
	}
	for _, n := range taken {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := h.callOK(t, "get_file", map[string]any{"file_id": testFileID, "name": "x.txt"})
	m := regexp.MustCompile(`^Saved (.*/x-[a-z0-9]{6}\.txt) \(file id: ` + testFileID + `\)$`).FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("got %q", got)
	}
	wantSaved(t, got, m[1], "body")
	if n := len(dirEntries(t, dir)); n != 101 {
		t.Fatalf("dir holds %d files, want 101", n)
	}
}

func TestGetFileRedirect(t *testing.T) {
	for _, tc := range []struct{ location, want string }{
		{"https://evil.example/steal", "https://evil.example/steal"},
		{"", "an undisclosed location"},
	} {
		dir := t.TempDir()
		h := newDownloadHarness(t, dir)
		h.fake.Handle(http.MethodGet, filePattern, func(w http.ResponseWriter, _ *http.Request) {
			if tc.location != "" {
				w.Header().Set("Location", tc.location)
			}
			w.WriteHeader(http.StatusFound)
		})
		text, isErr := h.callTool(t, "get_file", map[string]any{"file_id": testFileID})
		wantErr(t, text, isErr, "file download "+testFileID+" redirected to "+tc.want+"; refusing to follow it")
		if n := len(h.fake.Requests()); n != 1 {
			t.Fatalf("%d requests, want 1", n)
		}
		if names := dirEntries(t, dir); len(names) != 0 {
			t.Fatalf("dir holds %q", names)
		}
	}
}

func TestGetFileAPIError(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	serveFail(h, http.MethodGet, filePattern, http.StatusNotFound, "Unable to find the file.")
	text, isErr := h.callTool(t, "get_file", map[string]any{"file_id": testFileID})
	wantErr(t, text, isErr, "mattermost API 404 /api/v4/files/"+testFileID+": Unable to find the file.")

	h.fake.Handle(http.MethodGet, filePattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	})
	text, isErr = h.callTool(t, "get_file", map[string]any{"file_id": testFileID})
	wantErr(t, text, isErr, "mattermost API 502 /api/v4/files/"+testFileID+": "+mattermost.NoServerMessage)

	h.fake.Handle(http.MethodGet, filePattern, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "limit exceeded\n")
	})
	text, isErr = h.callTool(t, "get_file", map[string]any{"file_id": testFileID})
	wantErr(t, text, isErr, "mattermost API 429 /api/v4/files/"+testFileID+": limit exceeded")
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Fatalf("dir holds %q", names)
	}
}

func TestGetFileOversizeDeclared(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	h.fake.Handle(http.MethodGet, filePattern, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(maxDownload+1))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "short")
	})
	text, isErr := h.callTool(t, "get_file", map[string]any{"file_id": testFileID})
	wantErr(t, text, isErr, "file "+testFileID+" is 268435457 bytes, over the 268435456 bytes limit")
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Fatalf("dir holds %q", names)
	}
}

func TestGetFileOversizeDeclaredHuman(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	h.fake.Handle(http.MethodGet, filePattern, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(300<<20))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "short")
	})
	text, isErr := h.callTool(t, "get_file", map[string]any{"file_id": testFileID})
	wantErr(t, text, isErr, "file "+testFileID+" is 300.0 MB, over the 256.0 MB limit")
}

// zeros is an endless reader of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestGetFileOversizeStreamed(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	h.fake.Handle(http.MethodGet, filePattern, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="big.bin"`)
		w.WriteHeader(http.StatusOK) // chunked: no Content-Length
		_, _ = io.Copy(w, io.LimitReader(zeros{}, maxDownload+1<<20))
	})
	text, isErr := h.callTool(t, "get_file", map[string]any{"file_id": testFileID})
	wantErr(t, text, isErr, "file "+testFileID+" is over the 256.0 MB limit")
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Fatalf("partial file left: %q", names)
	}
}

func TestGetFileCancelRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	fake := testutil.New(t)
	mm := mattermost.NewContext(config.Config{URL: fake.URL, Token: testutil.Token, Team: testutil.TeamName, DownloadDir: dir})
	started := make(chan struct{})
	fake.Handle(http.MethodGet, filePattern, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.Repeat("x", 4096))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := getFile(ctx, mm, getFileIn{FileID: testFileID, Name: "part.txt"})
	if err == nil || !strings.HasPrefix(err.Error(), "file download "+testFileID+" failed: ") {
		t.Fatalf("err = %v", err)
	}
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Fatalf("partial file left: %q", names)
	}
}

func TestGetFileNoDownloadDir(t *testing.T) {
	h := newHarness(t)
	text, isErr := h.callTool(t, "get_file", map[string]any{"file_id": testFileID})
	wantErr(t, text, isErr, "get_file needs MM_MCP_DOWNLOAD_DIR: set it to an existing, writable directory")
	if n := len(h.fake.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestGetFileInvalidID(t *testing.T) {
	h := newDownloadHarness(t, t.TempDir())
	text, isErr := h.callTool(t, "get_file", map[string]any{"file_id": "../users/me"})
	wantErr(t, text, isErr, `invalid file id: "../users/me" (expected a 26-char id)`)
	if n := len(h.fake.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

func TestGetFileMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	h := newDownloadHarness(t, dir)
	serveFile(h, "", "body")
	text, isErr := h.callTool(t, "get_file", map[string]any{"file_id": testFileID, "name": "a.txt"})
	wantErr(t, text, isErr, "cannot save a.txt: open "+filepath.Join(dir, "a.txt")+": no such file or directory")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("directory was created: %v", err)
	}
}

func TestGetFileDescription(t *testing.T) {
	for _, tc := range []struct{ dir, want string }{
		{"/srv/mm-files", "into /srv/mm-files and"},
		{"", "into the MM_MCP_DOWNLOAD_DIR directory (currently unset, so every call fails) and"},
	} {
		h := newDownloadHarness(t, tc.dir)
		res, err := h.session.ListTools(context.Background(), &mcp.ListToolsParams{})
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(res.Tools, func(tool *mcp.Tool) bool { return tool.Name == "get_file" })
		if i < 0 || !strings.Contains(res.Tools[i].Description, tc.want) {
			t.Fatalf("description missing %q", tc.want)
		}
	}
}

func TestGetFileLongName(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	serveFile(h, "", "body")
	long := strings.Repeat("ж", 200) + ".txt" // 404 bytes, over NAME_MAX
	clipped := strings.Repeat("ж", 125) + ".txt"
	got := h.callOK(t, "get_file", map[string]any{"file_id": testFileID, "name": long})
	wantSaved(t, got, filepath.Join(dir, clipped), "body")

	// A name that fits only without a suffix still gets one on collision.
	fits := strings.Repeat("a", 251) + ".txt"
	got = h.callOK(t, "get_file", map[string]any{"file_id": testFileID, "name": fits})
	wantSaved(t, got, filepath.Join(dir, fits), "body")
	got = h.callOK(t, "get_file", map[string]any{"file_id": testFileID, "name": fits})
	wantSaved(t, got, filepath.Join(dir, strings.Repeat("a", 249)+"-1.txt"), "body")
}

func TestGetFileSymlinkNotFollowed(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.txt", "a-1.txt"} { // live and dangling
		target := victim
		if n == "a-1.txt" {
			target = filepath.Join(dir, "missing")
		}
		if err := os.Symlink(target, filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	h := newDownloadHarness(t, dir)
	serveFile(h, "", "evil")
	got := h.callOK(t, "get_file", map[string]any{"file_id": testFileID, "name": "a.txt"})
	wantSaved(t, got, filepath.Join(dir, "a-2.txt"), "evil")
	if data, _ := os.ReadFile(victim); string(data) != "keep" { //nolint:gosec // test path
		t.Fatalf("symlink followed: victim holds %q", data)
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("dangling symlink followed: %v", err)
	}
}

func TestGetFileTruncatedBodyRemovesPartial(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	h.fake.Handle(http.MethodGet, filePattern, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "only ten b")
	})
	text, isErr := h.callTool(t, "get_file", map[string]any{"file_id": testFileID, "name": "t.txt"})
	if !isErr || !strings.HasPrefix(text, "file download "+testFileID+" failed: ") {
		t.Fatalf("got %q (isError %v)", text, isErr)
	}
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Fatalf("partial file left: %q", names)
	}
}
