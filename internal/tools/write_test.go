package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

const (
	filesPath    = testutil.APIPrefix + "/files"
	postsPath    = testutil.APIPrefix + "/posts"
	schedulePath = testutil.APIPrefix + "/posts/schedule"
	outsideHint  = " — set " + config.EnvUploadRoot + " to allow it"
)

// newRootHarness is newHarness with the Context's upload root set to root.
func newRootHarness(t *testing.T, root string) *harness {
	t.Helper()
	return newConfigHarness(t, config.Config{UploadRoot: root})
}

// resolvedTempDir is t.TempDir with symlinks resolved, as config.Load delivers the root.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fileID is the id the fake upload handler gives a file named name.
func fileID(name string) string { return postID("f" + strings.TrimSuffix(name, filepath.Ext(name))) }

// serveUploads answers POST /files with one FileInfo per uploaded part, id from fileID; a file
// named in fail gets a 413 instead.
func serveUploads(t *testing.T, h *harness, fail ...string) {
	h.fake.Handle(http.MethodPost, "/files", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("upload form: %v", err)
		}
		if got := r.FormValue("channel_id"); got != testutil.TestChannelID {
			t.Errorf("upload channel_id %q", got)
		}
		var infos []*model.FileInfo
		for _, fh := range r.MultipartForm.File["files"] {
			if slices.Contains(fail, fh.Filename) {
				testutil.WriteError(w, http.StatusRequestEntityTooLarge, "test.error", "too big")
				return
			}
			infos = append(infos, &model.FileInfo{Id: fileID(fh.Filename), Name: fh.Filename})
		}
		testutil.WriteJSON(w, http.StatusCreated, &model.FileUploadResponse{FileInfos: infos})
	})
}

func servePosts201(h *harness) {
	h.fake.Handle(http.MethodPost, "/posts", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusCreated, &model.Post{Id: postID("new")})
	})
	h.fake.Handle(http.MethodPost, "/posts/schedule", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusCreated, &model.ScheduledPost{Id: postID("sched")})
	})
}

func serveMaxFileSize(h *harness, limit string) {
	h.fake.HandleClientConfig(func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, map[string]string{"MaxFileSize": limit})
	})
}

func wantNoRequests(t *testing.T, h *harness) {
	t.Helper()
	if got := calls(h, true); len(got) != 0 {
		t.Fatalf("requests sent: %q", got)
	}
}

// serveRoot answers GET /posts/{id} with root.
func serveRoot(h *harness, root *model.Post) {
	h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, root)
	})
}

func TestCreatePost(t *testing.T) {
	h := newHarness(t)
	servePosts201(h)
	serveRoot(h, &model.Post{Id: postID("root"), ChannelId: testutil.TestChannelID})
	got := h.callOK(t, "create_post", map[string]any{
		"channel": testutil.TestChannel, "message": "hi *there*", "thread_root_id": postID("root"),
	})
	if want := "Posted to mm-test in thread " + postID("root") + " (post id: " + postID("new") + ")"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	wantCalls(t, h, "POST "+postsPath)
	if got := calls(h, true); !slices.Contains(got, "GET "+postsPath+"/"+postID("root")) {
		t.Fatalf("thread root not read: %q", got)
	}
	var sent model.Post
	bodyOf(t, h, http.MethodPost, postsPath, &sent)
	if sent.ChannelId != testutil.TestChannelID || sent.Message != "hi *there*" || sent.RootId != postID("root") || len(sent.FileIds) != 0 {
		t.Fatalf("post sent: %+v", &sent)
	}
}

func TestCreatePostInDM(t *testing.T) {
	h := newHarness(t)
	servePosts201(h)
	got := h.callOK(t, "create_post", map[string]any{"channel": testutil.DMName, "message": "hi"})
	want := "Posted to " + testutil.DMName + " (DM with @" + testutil.OwnerName + ") (post id: " + postID("new") + ")"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCreatePostToUsername(t *testing.T) {
	h := newHarness(t)
	servePosts201(h)
	text, isErr := h.callTool(t, "create_post", map[string]any{"channel": "@" + testutil.OwnerName, "message": "hi"})
	wantErr(t, text, isErr, `"@ivan.ch" is a user, not a channel: open the DM with dm(username="ivan.ch") and pass the channel it returns`)
	wantCalls(t, h)
}

func TestCreatePostWithAttachments(t *testing.T) {
	root := resolvedTempDir(t)
	a := writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	writeFile(t, filepath.Join(root, "b.txt"), "bravo")
	t.Chdir(root)

	h := newRootHarness(t, root)
	serveUploads(t, h)
	servePosts201(h)
	serveMaxFileSize(h, "100")
	// Whitespace-only message is fine with attachments; relative paths resolve against the cwd.
	got := h.callOK(t, "create_post", map[string]any{
		"channel": testutil.TestChannel, "message": "  ", "attachments": []string{a, "b.txt"},
	})
	if want := "Posted to mm-test (post id: " + postID("new") + ", 2 files)"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	wantCalls(t, h, "POST "+filesPath, "POST "+filesPath, "POST "+postsPath)
	var sent model.Post
	bodyOf(t, h, http.MethodPost, postsPath, &sent)
	if !slices.Equal([]string(sent.FileIds), []string{fileID("a.txt"), fileID("b.txt")}) {
		t.Fatalf("file_ids sent: %q", sent.FileIds)
	}
	var bodies []string
	for _, r := range h.fake.Requests() {
		if r.Method == http.MethodPost && r.Path == filesPath {
			bodies = append(bodies, string(r.Body))
		}
	}
	if len(bodies) != 2 || !strings.Contains(bodies[0], "alpha") || !strings.Contains(bodies[1], "bravo") {
		t.Fatalf("upload bodies do not carry the file contents in order: %q", bodies)
	}
}

func TestCreatePostScheduled(t *testing.T) {
	root := resolvedTempDir(t)
	a := writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	h := newRootHarness(t, root)
	serveUploads(t, h)
	servePosts201(h)
	serveRoot(h, &model.Post{Id: postID("root"), ChannelId: testutil.TestChannelID})

	before := time.Now()
	got := h.callOK(t, "create_post", map[string]any{
		"channel": testutil.TestChannelID, "message": "later", "schedule_at": "2h", "attachments": []string{a},
		"thread_root_id": postID("root"),
	})
	wantCalls(t, h, "POST "+filesPath, "POST "+schedulePath)
	var sent model.ScheduledPost
	bodyOf(t, h, http.MethodPost, schedulePath, &sent)
	if d := time.UnixMilli(sent.ScheduledAt).Sub(before); d < 2*time.Hour-time.Second || d > 2*time.Hour+time.Minute {
		t.Fatalf("scheduled_at %d is %v from now", sent.ScheduledAt, d)
	}
	if sent.ChannelId != testutil.TestChannelID || sent.Message != "later" || sent.RootId != postID("root") ||
		!slices.Equal([]string(sent.FileIds), []string{fileID("a.txt")}) {
		t.Fatalf("scheduled post sent: %+v", &sent)
	}
	when := time.UnixMilli(sent.ScheduledAt).Local().Format("2006-01-02 15:04 MST")
	want := "Scheduled for " + when + " in mm-test in thread " + postID("root") + " (scheduled post id: " + postID("sched") + ", 1 file)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCreatePostScheduledNoThread(t *testing.T) {
	h := newHarness(t)
	servePosts201(h)
	got := h.callOK(t, "create_post", map[string]any{"channel": testutil.TestChannel, "message": "later", "schedule_at": "2h"})
	if !strings.HasSuffix(got, " in mm-test (scheduled post id: "+postID("sched")+")") {
		t.Fatalf("got %q", got)
	}
}

// TestCreatePostThreadRoot checks that a thread root the server would reject is refused before
// any upload.
func TestCreatePostThreadRoot(t *testing.T) {
	root := resolvedTempDir(t)
	a := writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	id := postID("root")
	tests := []struct {
		name  string
		serve func(h *harness)
		want  string
	}{
		{"not found", func(h *harness) {
			serveFail(h, http.MethodGet, "/posts/{post_id}", http.StatusNotFound, "Unable to get the post.")
		}, "thread root " + id + " not found — it is deleted, or in a channel this user cannot read"},
		{"other channel", func(h *harness) {
			serveRoot(h, &model.Post{Id: id, ChannelId: postID("other")})
		}, "thread root " + id + " is not in mm-test"},
		{"reply", func(h *harness) {
			serveRoot(h, &model.Post{Id: id, ChannelId: testutil.TestChannelID, RootId: postID("real")})
		}, "post " + id + " is a reply, not a thread root — its thread root id is " + postID("real")},
		{"other error", func(h *harness) {
			serveFail(h, http.MethodGet, "/posts/{post_id}", http.StatusForbidden, "denied")
		}, "mattermost API 403 " + postsPath + "/" + id + ": denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRootHarness(t, root)
			serveUploads(t, h)
			servePosts201(h)
			serveMaxFileSize(h, "100")
			tt.serve(h)
			text, isErr := h.callTool(t, "create_post", map[string]any{
				"channel": testutil.TestChannel, "message": "x", "thread_root_id": id, "attachments": []string{a},
			})
			wantErr(t, text, isErr, tt.want)
			wantCalls(t, h)
		})
	}
}

func TestCreatePostTooManyAttachments(t *testing.T) {
	h := newRootHarness(t, resolvedTempDir(t))
	files := make([]string, 11)
	for i := range files {
		files[i] = fmt.Sprintf("f%d.txt", i)
	}
	text, isErr := h.callTool(t, "create_post", map[string]any{
		"channel": testutil.TestChannel, "message": "x", "attachments": files,
	})
	wantErr(t, text, isErr, "too many attachments: 11 (a post carries at most 10 files)")
	wantNoRequests(t, h)
}

func TestCreatePostAttachmentsDisabled(t *testing.T) {
	h := newRootHarness(t, "")
	text, isErr := h.callTool(t, "create_post", map[string]any{
		"channel": testutil.TestChannel, "message": "hi", "attachments": []string{"a.txt"},
	})
	wantErr(t, text, isErr, "attachments are disabled: set MM_MCP_UPLOAD_ROOT to a directory to allow them")
	wantNoRequests(t, h)
}

func TestCreatePostEmptyMessage(t *testing.T) {
	for _, msg := range []string{"", " \n\t"} {
		h := newHarness(t)
		text, isErr := h.callTool(t, "create_post", map[string]any{"channel": testutil.TestChannel, "message": msg})
		wantErr(t, text, isErr, "refusing to post an empty message with no attachments")
		wantNoRequests(t, h)
	}
}

// TestCreatePostOrder checks that each step's failure wins over every later step's and that no
// request is sent before the checks it must follow.
func TestCreatePostOrder(t *testing.T) {
	root := resolvedTempDir(t)
	outside := writeFile(t, filepath.Join(t.TempDir(), "secret.txt"), "s")
	inside := writeFile(t, filepath.Join(root, "ok.txt"), "ok")
	missing := filepath.Join(root, "missing.txt")
	big := writeFile(t, filepath.Join(root, "big.txt"), strings.Repeat("x", 200))
	over := writeFile(t, filepath.Join(root, "over.txt"), strings.Repeat("x", 1025))

	t.Run("empty before schedule", func(t *testing.T) {
		h := newRootHarness(t, root)
		text, isErr := h.callTool(t, "create_post", map[string]any{"channel": "nope", "message": "", "schedule_at": "bad"})
		wantErr(t, text, isErr, "refusing to post an empty message with no attachments")
		wantNoRequests(t, h)
	})
	t.Run("schedule before attachments", func(t *testing.T) {
		h := newRootHarness(t, root)
		text, isErr := h.callTool(t, "create_post", map[string]any{
			"channel": "nope", "message": "x", "schedule_at": "-5m", "attachments": []string{outside},
		})
		if !isErr || strings.HasPrefix(text, "attachment") {
			t.Fatalf("want schedule error, got %q (isError %v)", text, isErr)
		}
		wantNoRequests(t, h)
	})
	t.Run("attachments before channel, in order", func(t *testing.T) {
		h := newRootHarness(t, root)
		text, isErr := h.callTool(t, "create_post", map[string]any{
			"channel": "nope", "message": "x", "attachments": []string{inside, outside, "/etc/passwd"},
		})
		wantErr(t, text, isErr, "attachment outside "+root+": "+outside+" (resolved to "+outside+")"+outsideHint)
		wantNoRequests(t, h)
	})
	t.Run("channel before existence", func(t *testing.T) {
		h := newRootHarness(t, root)
		text, isErr := h.callTool(t, "create_post", map[string]any{
			"channel": "nope", "message": "x", "attachments": []string{missing},
		})
		if !isErr || !strings.HasPrefix(text, "channel not found: nope") {
			t.Fatalf("got %q (isError %v)", text, isErr)
		}
		wantCalls(t, h)
	})
	t.Run("every file checked before any upload", func(t *testing.T) {
		h := newRootHarness(t, root)
		serveUploads(t, h)
		text, isErr := h.callTool(t, "create_post", map[string]any{
			"channel": testutil.TestChannel, "message": "x", "attachments": []string{inside, missing},
		})
		wantErr(t, text, isErr, "attachment not found: "+missing+" (resolved to "+missing+")")
		wantCalls(t, h)
	})
	t.Run("existence before size", func(t *testing.T) {
		h := newRootHarness(t, root)
		serveMaxFileSize(h, "100")
		text, isErr := h.callTool(t, "create_post", map[string]any{
			"channel": testutil.TestChannel, "message": "x", "attachments": []string{big, missing},
		})
		wantErr(t, text, isErr, "attachment not found: "+missing+" (resolved to "+missing+")")
		wantCalls(t, h)
	})
	t.Run("size before upload", func(t *testing.T) {
		h := newRootHarness(t, root)
		serveUploads(t, h)
		serveMaxFileSize(h, "100")
		text, isErr := h.callTool(t, "create_post", map[string]any{
			"channel": testutil.TestChannel, "message": "x", "attachments": []string{inside, big},
		})
		wantErr(t, text, isErr, "attachment too large: "+big+" is 200 B, over the server limit of 100 B")
		wantCalls(t, h)
	})
	t.Run("size at limit+1 shows exact bytes", func(t *testing.T) {
		h := newRootHarness(t, root)
		serveUploads(t, h)
		serveMaxFileSize(h, "1024")
		text, isErr := h.callTool(t, "create_post", map[string]any{
			"channel": testutil.TestChannel, "message": "x", "attachments": []string{over},
		})
		wantErr(t, text, isErr, "attachment too large: "+over+" is 1025 bytes, over the server limit of 1024 bytes")
		wantCalls(t, h)
	})
}

func TestCreatePostSizeUnknown(t *testing.T) {
	root := resolvedTempDir(t)
	big := writeFile(t, filepath.Join(root, "big.txt"), strings.Repeat("x", 200))
	for name, serve := range map[string]func(h *harness){
		"config fails":  func(_ *harness) {}, // the fake has no /config/client route: 404
		"no limit sent": func(h *harness) { serveMaxFileSize(h, "") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newRootHarness(t, root)
			serve(h)
			serveUploads(t, h)
			servePosts201(h)
			got := h.callOK(t, "create_post", map[string]any{
				"channel": testutil.TestChannel, "message": "x", "attachments": []string{big},
			})
			if !strings.HasSuffix(got, ", 1 file)") {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestCreatePostConfinement(t *testing.T) {
	root := resolvedTempDir(t)
	elsewhere := resolvedTempDir(t)
	secret := writeFile(t, filepath.Join(elsewhere, "secret.txt"), "s")
	inside := writeFile(t, filepath.Join(root, "ok.txt"), "ok")
	mustSymlink := func(target, link string) string {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		return link
	}
	escape := mustSymlink(secret, filepath.Join(root, "escape.txt"))
	escapeDir := mustSymlink(elsewhere, filepath.Join(root, "escape-dir"))
	good := mustSymlink(inside, filepath.Join(root, "good.txt"))
	dangling := mustSymlink(filepath.Join(elsewhere, "gone.txt"), filepath.Join(root, "dangling.txt"))
	loop := mustSymlink(filepath.Join(root, "loop"), filepath.Join(root, "loop"))
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// A file reached through a symlink to the root is inside it.
	rootLink := mustSymlink(root, filepath.Join(elsewhere, "root-link"))
	// A sibling whose name extends the root's is not inside it.
	evil := root + "-evil"
	if err := os.Mkdir(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	evilFile := writeFile(t, filepath.Join(evil, "x.txt"), "x")
	dotDotFile := writeFile(t, filepath.Join(root, "..dots.txt"), "d")
	relEscape := filepath.Join("..", filepath.Base(elsewhere), "secret.txt")
	t.Chdir(root)

	cases := []struct {
		name, root, path, want string // want "" = accepted
	}{
		{"symlink escape", root, escape, "attachment outside " + root + ": " + escape + " (resolved to " + secret + ")" + outsideHint},
		{"symlinked dir escape", root, filepath.Join(escapeDir, "secret.txt"),
			"attachment outside " + root + ": " + filepath.Join(escapeDir, "secret.txt") + " (resolved to " + secret + ")" + outsideHint},
		{"dot-dot escape", root, filepath.Join(root, "..", filepath.Base(elsewhere), "secret.txt"),
			"attachment outside " + root + ": " + filepath.Join(root, "..", filepath.Base(elsewhere), "secret.txt") +
				" (resolved to " + secret + ")" + outsideHint},
		{"root itself", root, root, "attachment is " + root + " itself, not a file: " + root},
		{"missing is not an escape", root, dangling, "attachment not found: " + dangling + " (resolved to " + dangling + ")"},
		{"loop refused", root, loop, "attachment cannot be resolved: " + loop + " (EvalSymlinks: too many links)"},
		{"directory", root, sub, "attachment is not a regular file: " + sub + " (resolved to " + sub + ")"},
		{"fifo", root, fifo, "attachment is not a regular file: " + fifo + " (resolved to " + fifo + ")"},
		{"symlink inside", root, good, ""},
		{"file via symlinked root", root, filepath.Join(rootLink, "ok.txt"), ""},
		{"slash allows any file", "/", secret, ""},
		{"prefix sibling", root, evilFile, "attachment outside " + root + ": " + evilFile + " (resolved to " + evilFile + ")" + outsideHint},
		{"relative dot-dot escape", root, relEscape, "attachment outside " + root + ": " + relEscape + " (resolved to " + secret + ")" + outsideHint},
		{"relative inside", root, "ok.txt", ""},
		{"name starting with dots", root, dotDotFile, ""},
		{"device", "/", "/dev/null", "attachment is not a regular file: /dev/null (resolved to /dev/null)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRootHarness(t, tc.root)
			serveUploads(t, h)
			servePosts201(h)
			text, isErr := h.callTool(t, "create_post", map[string]any{
				"channel": testutil.TestChannel, "message": "x", "attachments": []string{tc.path},
			})
			if tc.want == "" {
				if isErr {
					t.Fatalf("refused: %s", text)
				}
				return
			}
			wantErr(t, text, isErr, tc.want)
			wantCalls(t, h)
		})
	}
}

// TestCreatePostRootSwappedForSymlink checks that the root resolved at startup is not resolved
// again: a symlink swapped in for it later does not let files outside the original root in.
func TestCreatePostRootSwappedForSymlink(t *testing.T) {
	root := resolvedTempDir(t)
	elsewhere := resolvedTempDir(t)
	secret := writeFile(t, filepath.Join(elsewhere, "secret.txt"), "s")
	h := newRootHarness(t, root)
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "secret.txt")
	text, isErr := h.callTool(t, "create_post", map[string]any{
		"channel": testutil.TestChannel, "message": "x", "attachments": []string{path},
	})
	wantErr(t, text, isErr, "attachment outside "+root+": "+path+" (resolved to "+secret+")"+outsideHint)
	wantCalls(t, h)
}

// TestOpenAllComponentSwapped checks that a directory swapped for a symlink out of the root
// between the check and the open does not let a file outside the root be opened.
func TestOpenAllComponentSwapped(t *testing.T) {
	root := resolvedTempDir(t)
	elsewhere := resolvedTempDir(t)
	for _, d := range []string{root, elsewhere} {
		if err := os.Mkdir(filepath.Join(d, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := writeFile(t, filepath.Join(root, "sub", "f.txt"), "inside")
	writeFile(t, filepath.Join(elsewhere, "sub", "f.txt"), "outside")
	h := newRootHarness(t, root)
	dir, files, err := confine(root, []string{path})
	if err != nil {
		t.Fatalf("confine: %v", err)
	}
	defer func() { _ = dir.Close() }()
	defer closeAll(files)
	if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub-moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "sub"), filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	err = openAll(t.Context(), h.mm, dir, files)
	want := "attachment cannot be read: " + path + " (path escapes from parent)"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if files[0].file != nil {
		t.Fatal("a file was opened")
	}
}

// TestSameRootSwappedBack checks that a handle opened while the root was a symlink out of it is
// refused once the root is swapped back, and that the root's own handle is accepted.
func TestSameRootSwappedBack(t *testing.T) {
	root := resolvedTempDir(t)
	elsewhere := resolvedTempDir(t)
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Fatal(err)
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root+"-moved", root); err != nil {
		t.Fatal(err)
	}
	err = sameRoot(dir, root)
	want := "attachment root changed while being checked: " + root
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}

	own, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = own.Close() }()
	if err := sameRoot(own, root); err != nil {
		t.Fatalf("own root: %v", err)
	}
}

// TestCreatePostRootRecreated checks that the root is opened per call: after it is removed and
// created again, its new files upload; while it is gone, the call says so.
func TestCreatePostRootRecreated(t *testing.T) {
	root := resolvedTempDir(t)
	h := newRootHarness(t, root)
	serveUploads(t, h)
	servePosts201(h)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a.txt")
	args := map[string]any{"channel": testutil.TestChannel, "message": "x", "attachments": []string{path}}
	text, isErr := h.callTool(t, "create_post", args)
	wantErr(t, text, isErr, "attachment root cannot be opened: "+root+" (no such file or directory)")
	wantNoRequests(t, h)

	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "alpha")
	if got := h.callOK(t, "create_post", args); !strings.HasSuffix(got, ", 1 file)") {
		t.Fatalf("got %q", got)
	}
}

func TestCreatePostUnreadableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	root := resolvedTempDir(t)
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, filepath.Join(locked, "f.txt"), "x")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	h := newRootHarness(t, root)
	text, isErr := h.callTool(t, "create_post", map[string]any{
		"channel": testutil.TestChannel, "message": "x", "attachments": []string{path},
	})
	wantErr(t, text, isErr, "attachment cannot be resolved: "+path+" (permission denied)")
	wantNoRequests(t, h)
}

func TestCreatePostFailures(t *testing.T) {
	root := resolvedTempDir(t)
	a := writeFile(t, filepath.Join(root, "a.txt"), "alpha")
	b := writeFile(t, filepath.Join(root, "b.txt"), "bravo")
	orphan := func(files, what, ids, cause string) string {
		return fmt.Sprintf("uploaded %s, then %s failed — file ids %s are orphaned on the server and cannot be "+
			"deleted (Mattermost only deletes a file with the post that carries it); cause: %s", files, what, ids, cause)
	}
	cases := []struct {
		name     string
		schedule bool
		files    []string
		failFile string // upload that fails
		failPost bool
		want     string
		calls    []string
	}{
		{"first upload fails", false, []string{a, b}, "a.txt", false,
			"mattermost API 413 " + filesPath + ": too big", []string{"POST " + filesPath}},
		{"second upload fails", false, []string{a, b}, "b.txt", false,
			orphan("1 file", b, fileID("a.txt"), "mattermost API 413 "+filesPath+": too big"),
			[]string{"POST " + filesPath, "POST " + filesPath}},
		{"post fails after uploads", false, []string{a, b}, "", true,
			orphan("2 files", "the post", fileID("a.txt")+", "+fileID("b.txt"), "mattermost API 403 "+postsPath+": denied"),
			[]string{"POST " + filesPath, "POST " + filesPath, "POST " + postsPath}},
		{"post fails without uploads", false, nil, "", true,
			"mattermost API 403 " + postsPath + ": denied", []string{"POST " + postsPath}},
		{"schedule fails after upload", true, []string{a}, "", true,
			orphan("1 file", "the post", fileID("a.txt"), "mattermost API 403 "+schedulePath+": denied"),
			[]string{"POST " + filesPath, "POST " + schedulePath}},
		{"schedule fails without uploads", true, nil, "", true,
			"mattermost API 403 " + schedulePath + ": denied", []string{"POST " + schedulePath}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRootHarness(t, root)
			if tc.failFile != "" {
				serveUploads(t, h, tc.failFile)
			} else {
				serveUploads(t, h)
			}
			servePosts201(h)
			if tc.failPost {
				serveFail(h, http.MethodPost, "/posts", http.StatusForbidden, "denied")
				serveFail(h, http.MethodPost, "/posts/schedule", http.StatusForbidden, "denied")
			}
			args := map[string]any{"channel": testutil.TestChannel, "message": "x", "attachments": tc.files}
			if tc.schedule {
				args["schedule_at"] = "1d"
			}
			text, isErr := h.callTool(t, "create_post", args)
			wantErr(t, text, isErr, tc.want)
			wantCalls(t, h, tc.calls...)
		})
	}
}

func TestCreatePostSchema(t *testing.T) {
	h := newHarness(t)
	checkSchema(t, h.toolSchemas(t), "create_post",
		[]string{"channel", "message"}, []string{"channel", "message", "thread_root_id", "schedule_at", "attachments"})
	res, err := h.session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	i := slices.IndexFunc(res.Tools, func(tool *mcp.Tool) bool { return tool.Name == "create_post" })
	if d := res.Tools[i].Description; !strings.Contains(d, "DELETE /posts/schedule/<id>") ||
		!strings.Contains(d, "GET /posts/scheduled/team/{team_id}?includeDirectChannels=true") {
		t.Errorf("description does not say how to list and cancel scheduled posts: %s", d)
	}
}

// TestUploadReadsCheckedSize checks that an attachment that grew after its size was checked is
// uploaded only up to that size.
func TestUploadReadsCheckedSize(t *testing.T) {
	h := newHarness(t)
	var got []byte
	h.fake.Handle(http.MethodPost, "/files", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("upload form: %v", err)
		}
		fh := r.MultipartForm.File["files"][0]
		f, err := fh.Open()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = f.Close() }()
		got, _ = io.ReadAll(f)
		testutil.WriteJSON(w, http.StatusCreated, &model.FileUploadResponse{FileInfos: []*model.FileInfo{{Id: fileID("a")}}})
	})
	path := writeFile(t, filepath.Join(resolvedTempDir(t), "a.txt"), "checked+grown")
	file, err := os.Open(path) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	att := &attachment{arg: path, abs: path, real: path, file: file, size: int64(len("checked"))}
	if _, err := uploadAll(t.Context(), h.mm, testutil.TestChannelID, []*attachment{att}); err != nil {
		t.Fatal(err)
	}
	if string(got) != "checked" {
		t.Fatalf("uploaded %q, want %q", got, "checked")
	}
}

// TestUploadStreamsMultipart checks the multipart request a file larger than any small buffer
// is streamed as: the channel_id field, then the file part with its name and full contents.
func TestUploadStreamsMultipart(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodPost, "/files", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusCreated, &model.FileUploadResponse{FileInfos: []*model.FileInfo{{Id: fileID("big")}}})
	})
	content := strings.Repeat("0123456789abcdef", 3<<16) // 3 MiB
	path := writeFile(t, filepath.Join(resolvedTempDir(t), "big.bin"), content)
	file, err := os.Open(path) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	att := &attachment{arg: path, abs: path, real: path, file: file, size: int64(len(content))}
	ids, err := uploadAll(t.Context(), h.mm, testutil.TestChannelID, []*attachment{att})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{fileID("big")}) {
		t.Fatalf("ids %q", ids)
	}

	reqs := h.fake.Requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != filesPath {
		t.Fatalf("requests: %+v", reqs)
	}
	_, params, err := mime.ParseMediaType(reqs[0].Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(reqs[0].Body), params["boundary"])
	var parts []string
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		switch p.FormName() {
		case "channel_id":
			if string(data) != testutil.TestChannelID {
				t.Errorf("channel_id %q", data)
			}
		case "files":
			if p.FileName() != "big.bin" || string(data) != content {
				t.Errorf("file part %q with %d bytes", p.FileName(), len(data))
			}
		}
		parts = append(parts, p.FormName())
	}
	if !slices.Equal(parts, []string{"channel_id", "files"}) {
		t.Fatalf("parts %q", parts)
	}
}

// TestUploadReadFailure checks that an attachment that can't supply its checked size is reported
// as unreadable, not as a server or transport error.
func TestUploadReadFailure(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *os.File) int64
		cause string
	}{
		{"closed", func(f *os.File) int64 { _ = f.Close(); return 5 }, "file already closed"},
		{"shrank", func(*os.File) int64 { return 100 }, "unexpected EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.fake.Handle(http.MethodPost, "/files", func(w http.ResponseWriter, _ *http.Request) {
				testutil.WriteJSON(w, http.StatusCreated, &model.FileUploadResponse{})
			})
			path := writeFile(t, filepath.Join(resolvedTempDir(t), "a.txt"), "alpha")
			file, err := os.Open(path) //nolint:gosec // test file
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			att := &attachment{arg: path, abs: path, real: path, file: file, size: tt.setup(file)}
			_, err = uploadAll(t.Context(), h.mm, testutil.TestChannelID, []*attachment{att})
			want := "attachment cannot be read: " + path + " (" + tt.cause + ")"
			if err == nil || err.Error() != want {
				t.Fatalf("got %v, want %q", err, want)
			}
		})
	}
}
