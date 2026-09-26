package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

// e2eAnnotations is the expected annotation table: readOnly, destructive, idempotent. openWorld is true for
// every tool. Kept independent of the annotations map so a table typo cannot hide itself.
var e2eAnnotations = map[string][3]bool{
	"list_channels":   {true, false, false},
	"read_posts":      {true, false, false},
	"get_post":        {true, false, false},
	"search":          {true, false, false},
	"list_members":    {true, false, false},
	"get_file":        {false, false, false},
	"follow_thread":   {false, false, true},
	"unfollow_thread": {false, false, true},
	"create_post":     {false, false, false},
	"react":           {false, true, true},
	"edit_post":       {false, true, false},
	"dm":              {false, false, true},
	"api":             {false, true, false},
}

// TestE2EListTools checks tools/list over the in-memory transport: exactly the 15 tools,
// their annotations, and a description on every tool and input property.
func TestE2EListTools(t *testing.T) {
	h := newHarness(t)
	list, err := h.session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		want, ok := e2eAnnotations[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("%s: no description", tool.Name)
		}
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s: no annotations", tool.Name)
			continue
		}
		if a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("%s: openWorldHint = %v, want true", tool.Name, a.OpenWorldHint)
		}
		if a.ReadOnlyHint != want[0] {
			t.Errorf("%s: readOnlyHint = %v, want %v", tool.Name, a.ReadOnlyHint, want[0])
		}
		// destructiveHint defaults to true when absent, so a non-read-only tool must set it.
		destructive := a.DestructiveHint == nil || *a.DestructiveHint
		if !want[0] && destructive != want[1] {
			t.Errorf("%s: destructiveHint = %v, want %v", tool.Name, destructive, want[1])
		}
		if a.IdempotentHint != want[2] {
			t.Errorf("%s: idempotentHint = %v, want %v", tool.Name, a.IdempotentHint, want[2])
		}

		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s: schema: %v", tool.Name, err)
		}
		var schema struct {
			Type       string `json:"type"`
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: schema %s: %v", tool.Name, raw, err)
		}
		if schema.Type != "object" {
			t.Errorf("%s: schema type %q, want object", tool.Name, schema.Type)
		}
		for prop, p := range schema.Properties {
			if strings.TrimSpace(p.Description) == "" {
				t.Errorf("%s.%s: no description", tool.Name, prop)
			}
		}
	}
	if len(names) != len(e2eAnnotations) {
		slices.Sort(names)
		t.Errorf("tools/list returned %d tools, want %d: %q", len(names), len(e2eAnnotations), names)
	}
}

// TestE2ECallEachTool calls every tool once, successfully, on one client session against the
// fake.
func TestE2ECallEachTool(t *testing.T) {
	dir := t.TempDir()
	h := newDownloadHarness(t, dir)
	f := h.fake

	post := testPost(postID("p1"), testutil.TestChannelID, testutil.OtherID, "hello", time.Hour)
	servePosts(h, "/channels/{channel_id}/posts", postList(post))
	f.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, r *http.Request) {
		if id := r.PathValue("post_id"); id != post.Id {
			testutil.WriteJSON(w, http.StatusOK, &model.Post{Id: id, ChannelId: testutil.TestChannelID, UserId: testutil.MeID, ReplyCount: 1})
			return
		}
		testutil.WriteJSON(w, http.StatusOK, post)
	})
	f.Handle(http.MethodPost, "/teams/{team_id}/posts/search", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, model.NewPostList())
	})
	f.Handle(http.MethodGet, "/users", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, []*model.User{{Id: testutil.OwnerID, Username: testutil.OwnerName}})
	})
	serveFile(h, `attachment; filename="report.txt"`, "payload")
	serveOK(h, http.MethodPut, "/users/{user_id}/teams/{team_id}/threads/{thread_id}/following")
	serveOK(h, http.MethodDelete, "/users/{user_id}/teams/{team_id}/threads/{thread_id}/following")
	servePosts201(h)
	f.Handle(http.MethodPost, "/reactions", func(w http.ResponseWriter, r *http.Request) {
		var in model.Reaction
		_ = json.NewDecoder(r.Body).Decode(&in)
		testutil.WriteJSON(w, http.StatusOK, in)
	})
	f.Handle(http.MethodPut, "/posts/{post_id}/patch", func(w http.ResponseWriter, r *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, testPost(r.PathValue("post_id"), testutil.TestChannelID, testutil.MeID, "new", 0))
	})
	f.Handle(http.MethodPost, "/channels/direct", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusCreated, fakeChannel(t, h, testutil.DMID))
	})
	serveRaw(h, http.MethodGet, "/users/me/status", http.StatusOK, `{"status":"online"}`)

	root := postID("root")
	cases := []struct {
		tool string
		args map[string]any
		want string // exact result text
	}{
		{"list_channels", nil, "- town-square — Town Square [O]\n- mm-test — MM Test [O]\n- dev-ops — Dev Ops [P]\n- " +
			testutil.DMName + " — DM with @" + testutil.OwnerName + " [D]"},
		{"read_posts", map[string]any{"channel": testutil.TestChannel}, "**bob** (1h ago): hello (post " + post.Id + ")"},
		{"get_post", map[string]any{"post_id": post.Id}, "in " + testutil.TestChannel + ":\n**bob** (1h ago): hello (post " + post.Id + ")"},
		{"search", map[string]any{"query": "nothing", "type": "posts"}, `No posts found for "nothing".`},
		{"list_members", map[string]any{"channel": testutil.TestChannel}, "- " + testutil.OwnerName},
		{"get_file", map[string]any{"file_id": testFileID},
			"Saved " + filepath.Join(dir, "report.txt") + " (file id: " + testFileID + ")"},
		{"follow_thread", map[string]any{"thread_root_id": root}, "Following thread " + root},
		{"unfollow_thread", map[string]any{"thread_root_id": root},
			"Left thread " + root + " — posting in it again re-follows it."},
		{"create_post", map[string]any{"channel": testutil.TestChannel, "message": "hi"},
			"Posted to mm-test (post id: " + postID("new") + ")"},
		{"react", map[string]any{"post_id": post.Id, "emoji": "thumbsup", "action": "add"},
			"Added :thumbsup: on post " + post.Id},
		{"edit_post", map[string]any{"post_id": root, "action": "edit", "message": "new"}, "Edited post " + root},
		{"dm", map[string]any{"username": testutil.OwnerName},
			"DM channel with @ivan.ch: " + testutil.DMName + " (id: " + testutil.DMID + ") — pass it as the channel to create_post"},
		{"api", map[string]any{"path": "/users/me/status"}, `{"status":"online"}`},
	}
	called := map[string]bool{}
	for _, tc := range cases {
		called[tc.tool] = true
		got := h.callOK(t, tc.tool, tc.args)
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.tool, got, tc.want)
		}
	}
	for name := range e2eAnnotations {
		if !called[name] {
			t.Errorf("tool %s not called", name)
		}
	}
}

// TestE2EToolError checks that a failing call comes back as an isError result carrying the
// API error sentence, not as a protocol error.
func TestE2EToolError(t *testing.T) {
	h := newHarness(t)
	id := postID("gone")
	h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusInternalServerError, "app.post.get.app_error", "Unable to get the post.")
	})
	text, isErr := h.callTool(t, "get_post", map[string]any{"post_id": id})
	wantErr(t, text, isErr, "mattermost API 500 "+testutil.APIPrefix+"/posts/"+id+": Unable to get the post.")
}
