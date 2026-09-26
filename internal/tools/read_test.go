package tools

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

// lastRequest returns the last request the fake received for path (full, with /api/v4).
func lastRequest(t *testing.T, h *harness, path string) testutil.Request {
	t.Helper()
	reqs := h.fake.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Path == path {
			return reqs[i]
		}
	}
	t.Fatalf("no request to %s", path)
	return testutil.Request{}
}

func servePosts(h *harness, pattern string, list *model.PostList) {
	h.fake.Handle(http.MethodGet, pattern, func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, list)
	})
}

func TestListChannels(t *testing.T) {
	h := newHarness(t)
	h.fake.Channels[0].DisplayName = ""
	selfDM := testutil.MeID + "__" + testutil.MeID
	nobody := postID("nobody")
	strangerDM := nobody + "__" + testutil.MeID
	h.fake.Channels = append(h.fake.Channels,
		&model.Channel{Id: postID("selfdm"), Name: selfDM, Type: model.ChannelTypeDirect},
		&model.Channel{Id: postID("strangerdm"), Name: strangerDM, Type: model.ChannelTypeDirect})
	dmLabels := map[string]string{
		testutil.DMName: "DM with @" + testutil.OwnerName,
		selfDM:          "DM with yourself",
		strangerDM:      "DM with @" + nobody, // unknown to the server: shown by id
	}
	text := h.callOK(t, "list_channels", nil)
	lines := strings.Split(text, "\n")
	if len(lines) != len(h.fake.Channels) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(h.fake.Channels), text)
	}
	for i, ch := range h.fake.Channels {
		display := ch.DisplayName
		if label, ok := dmLabels[ch.Name]; ok {
			display = label
		} else if display == "" { // the cleared first channel
			display = ch.Name
		}
		want := "- " + ch.Name + " — " + display + " [" + string(ch.Type) + "]"
		if lines[i] != want {
			t.Errorf("line %d: got %q, want %q", i, lines[i], want)
		}
	}
	// The DM members are looked up in one request.
	var lookups int
	for _, call := range calls(h, true) {
		if call == "POST "+testutil.APIPrefix+"/users/ids" {
			lookups++
		}
	}
	if lookups != 1 {
		t.Errorf("got %d user lookups, want 1", lookups)
	}
}

func TestListChannelsEmpty(t *testing.T) {
	h := newHarness(t)
	h.fake.Channels = nil
	if got := h.callOK(t, "list_channels", nil); got != "No channels found." {
		t.Fatalf("got %q", got)
	}
}

func TestListChannelsAPIError(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodGet, "/users/{user_id}/teams/{team_id}/channels", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusInternalServerError, "x", "boom")
	})
	text, isErr := h.callTool(t, "list_channels", nil)
	wantErr(t, text, isErr, "mattermost API 500 /api/v4/users/me/teams/"+testutil.TeamID+"/channels: boom")
}

func TestReadPostsLatest(t *testing.T) {
	h := newHarness(t)
	p1 := testPost(postID("p1"), testutil.TestChannelID, testutil.OtherID, "older", 2*time.Hour)
	p2 := testPost(postID("p2"), testutil.TestChannelID, testutil.MeID, "newer", time.Minute)
	list := postList(p2, p1)
	list.PrevPostId = postID("p0")
	servePosts(h, "/channels/{channel_id}/posts", list)

	text := h.callOK(t, "read_posts", map[string]any{"channel": testutil.TestChannel})
	want := "**bob** (2h ago): older (post " + p1.Id + ")\n**alice** (1m ago): newer (post " + p2.Id + ")\n" +
		"(2 posts shown — older posts exist, pass before=" + p1.Id + ")"
	if text != want {
		t.Fatalf("got:\n%s\nwant:\n%s", text, want)
	}
	req := lastRequest(t, h, "/api/v4/channels/"+testutil.TestChannelID+"/posts")
	if got := req.Query.Get("per_page"); got != "30" {
		t.Errorf("per_page = %q, want default 30", got)
	}
	if got := req.Query.Get("skipFetchThreads"); got != "true" {
		t.Errorf("skipFetchThreads = %q, want true (thread bodies are not rendered)", got)
	}
}

func TestReadPostsUsersLookupFails(t *testing.T) {
	h := newHarness(t)
	p := testPost(postID("p1"), testutil.TestChannelID, testutil.OtherID, "hello", time.Minute)
	servePosts(h, "/channels/{channel_id}/posts", postList(p))
	h.fake.Handle(http.MethodPost, "/users/ids", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusInternalServerError, "x", "db down")
	})
	text := h.callOK(t, "read_posts", map[string]any{"channel": testutil.TestChannel})
	if want := "**" + testutil.OtherID + "** (1m ago): hello (post " + p.Id + ")"; text != want {
		t.Fatalf("got %q, want %q", text, want)
	}
}

func TestReadPostsEmpty(t *testing.T) {
	h := newHarness(t)
	servePosts(h, "/channels/{channel_id}/posts", postList())
	text := h.callOK(t, "read_posts", map[string]any{"channel": testutil.TestChannel})
	if text != "(no posts)" {
		t.Fatalf("got %q", text)
	}
}

func TestReadPostsBefore(t *testing.T) {
	h := newHarness(t)
	p := testPost(postID("p1"), testutil.TestChannelID, testutil.OtherID, "old", time.Hour)
	servePosts(h, "/channels/{channel_id}/posts", postList(p))
	before := postID("p9")
	text := h.callOK(t, "read_posts", map[string]any{
		"channel": testutil.TestChannelID, "before": before, "limit": 5, "since": "1h",
	})
	if text != "**bob** (1h ago): old (post "+p.Id+")" {
		t.Fatalf("got %q", text)
	}
	req := lastRequest(t, h, "/api/v4/channels/"+testutil.TestChannelID+"/posts")
	if req.Query.Get("before") != before || req.Query.Get("per_page") != "5" || req.Query.Has("since") ||
		req.Query.Get("skipFetchThreads") != "true" {
		t.Errorf("query = %v, want before=%s per_page=5 skipFetchThreads=true and no since", req.Query, before)
	}
}

// serveSincePage serves a full page for since=4h&limit=2, newest first: posts "two" (2h ago) and
// "one" (3h ago), the extra post p0 created p0Age ago, and older posts beyond. It returns one
// and two.
func serveSincePage(h *harness, p0Age time.Duration) (p1, p2 *model.Post) {
	p0 := testPost(postID("p0"), testutil.TestChannelID, testutil.OtherID, "zero", p0Age)
	p1 = testPost(postID("p1"), testutil.TestChannelID, testutil.OtherID, "one", 3*time.Hour)
	p2 = testPost(postID("p2"), testutil.TestChannelID, testutil.OtherID, "two", 2*time.Hour)
	list := postList(p2, p1, p0)
	list.PrevPostId = postID("pp")
	servePosts(h, "/channels/{channel_id}/posts", list)
	return p1, p2
}

func TestReadPostsSincePages(t *testing.T) {
	h := newHarness(t)
	p1, p2 := serveSincePage(h, 210*time.Minute)

	text := h.callOK(t, "read_posts", map[string]any{"channel": testutil.TestChannel, "since": "4h", "limit": 2})
	want := "**bob** (3h ago): one (post " + p1.Id + ")\n**bob** (2h ago): two (post " + p2.Id + ")\n" +
		"(2 posts shown — older posts exist, pass before=" + p1.Id + ")"
	if text != want {
		t.Fatalf("got:\n%s\nwant:\n%s", text, want)
	}
	req := lastRequest(t, h, "/api/v4/channels/"+testutil.TestChannelID+"/posts")
	if req.Query.Get("per_page") != "3" || req.Query.Get("page") != "0" || req.Query.Has("since") ||
		req.Query.Get("skipFetchThreads") != "true" {
		t.Errorf("query = %v, want page=0 per_page=3 skipFetchThreads=true and no since", req.Query)
	}
}

// A full page whose extra post is before since has nothing more in the window: no hint.
func TestReadPostsSinceFullPageEnds(t *testing.T) {
	h := newHarness(t)
	p1, p2 := serveSincePage(h, 5*time.Hour)

	text := h.callOK(t, "read_posts", map[string]any{"channel": testutil.TestChannel, "since": "4h", "limit": 2})
	if want := "**bob** (3h ago): one (post " + p1.Id + ")\n**bob** (2h ago): two (post " + p2.Id + ")"; text != want {
		t.Fatalf("got:\n%s\nwant:\n%s", text, want)
	}
}

// The server caps per_page at 200, so the extra post is not asked for at limit=200.
func TestReadPostsSinceMaxLimit(t *testing.T) {
	h := newHarness(t)
	servePosts(h, "/channels/{channel_id}/posts", postList())
	h.callOK(t, "read_posts", map[string]any{"channel": testutil.TestChannel, "since": "4h", "limit": maxLimit})
	req := lastRequest(t, h, "/api/v4/channels/"+testutil.TestChannelID+"/posts")
	if got := req.Query.Get("per_page"); got != strconv.Itoa(maxLimit) {
		t.Errorf("per_page = %s, want %d", got, maxLimit)
	}
}

// An old post bumped by a reaction or reply must not show, and reaching past since ends paging.
func TestReadPostsSinceCutsOld(t *testing.T) {
	h := newHarness(t)
	old := testPost(postID("p1"), testutil.TestChannelID, testutil.OtherID, "weeks old", 21*24*time.Hour)
	old.UpdateAt = time.Now().UnixMilli()
	p2 := testPost(postID("p2"), testutil.TestChannelID, testutil.OtherID, "new", 30*time.Minute)
	list := postList(p2, old)
	list.PrevPostId = postID("p0")
	servePosts(h, "/channels/{channel_id}/posts", list)

	text := h.callOK(t, "read_posts", map[string]any{"channel": testutil.TestChannel, "since": "1h"})
	if text != "**bob** (30m ago): new (post "+p2.Id+")" {
		t.Fatalf("got %q", text)
	}
}

func TestReadPostsSinceInvalid(t *testing.T) {
	h := newHarness(t)
	text, isErr := h.callTool(t, "read_posts", map[string]any{"channel": testutil.TestChannel, "since": "yesterday"})
	wantErr(t, text, isErr, `invalid since: "yesterday" (use "2h", "30m", "45s", "3d", ISO 8601 date, or epoch ms)`)
	for _, r := range h.fake.Requests() {
		if strings.HasSuffix(r.Path, "/posts") {
			t.Errorf("posts fetched despite the invalid since: %s", r.Path)
		}
	}
}

func TestReadPostsPinned(t *testing.T) {
	h := newHarness(t)
	p1 := testPost(postID("p1"), testutil.TestChannelID, testutil.OtherID, "pin one", 2*time.Hour)
	p2 := testPost(postID("p2"), testutil.TestChannelID, testutil.OtherID, "pin two", time.Hour)
	servePosts(h, "/channels/{channel_id}/pinned", postList(p2, p1))

	cases := []struct {
		name  string
		limit int
		want  string
	}{
		{"trimmed", 1, "**bob** (1h ago): pin two (post " + p2.Id + ")\n(newest 1 of 2 pinned posts shown — pass limit=200 for more)"},
		{"all shown", 2, "**bob** (2h ago): pin one (post " + p1.Id + ")\n**bob** (1h ago): pin two (post " + p2.Id + ")"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// before is ignored: pinned outranks it, so the note must not offer before= paging.
			text := h.callOK(t, "read_posts", map[string]any{
				"channel": testutil.TestChannel, "pinned": true, "before": postID("p9"), "limit": tc.limit,
			})
			if text != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", text, tc.want)
			}
		})
	}
}

func TestPinnedNoteAtMaxLimit(t *testing.T) {
	var posts []*model.Post
	for i := range maxLimit + 1 {
		posts = append(posts, testPost(postID("p"+strconv.Itoa(i)), testutil.TestChannelID, testutil.OtherID, "pin", time.Duration(i)*time.Minute))
	}
	got := pinnedNote(postList(posts...), maxLimit)
	want := "\n(newest 200 of 201 pinned posts shown — older pinned posts are out of reach)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func threadList(root *model.Post, hasNext bool, replies ...*model.Post) *model.PostList {
	list := postList(append(replies, root)...)
	list.HasNext = &hasNext
	return list
}

func TestReadPostsThread(t *testing.T) {
	rootID := postID("root")
	root := testPost(rootID, testutil.TestChannelID, testutil.OtherID, "question", 3*time.Hour)
	r1 := testPost(postID("r1"), testutil.TestChannelID, testutil.MeID, "answer one", 2*time.Hour)
	r2 := testPost(postID("r2"), testutil.TestChannelID, testutil.MeID, "answer two", time.Hour)
	for _, r := range []*model.Post{r1, r2} {
		r.RootId = rootID
	}
	body := "**bob** (3h ago): question (post " + rootID + ")\n" +
		"  ↳ **alice** (2h ago): answer one (post " + r1.Id + ", thread " + rootID + ")\n" +
		"  ↳ **alice** (1h ago): answer two (post " + r2.Id + ", thread " + rootID + ")"

	cases := []struct {
		name       string
		limit      int
		hasNext    bool
		replyCount int64
		note       string
	}{
		{"fewer than limit", 3, false, 2, ""},
		// The server counts the root in its perPage+1 window: exactly limit replies still sets has_next.
		{"exactly limit replies", 2, true, 2, ""},
		{"exactly max limit replies", 200, true, 200, ""},
		{"more replies", 2, true, 5, "\n(newest 2 replies shown of 5 — pass limit=200 for more)"},
		{"more, count unknown", 2, true, 0, "\n(newest 2 replies shown — pass limit=200 for more)"},
		{"at max limit", 200, true, 300, "\n(newest 200 replies shown of 300 — older replies are out of reach)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			root.ReplyCount = tc.replyCount
			servePosts(h, "/posts/{post_id}/thread", threadList(root, tc.hasNext, r2, r1))
			text := h.callOK(t, "read_posts", map[string]any{
				"channel": testutil.TestChannel, "thread_root_id": rootID, "pinned": true, "limit": tc.limit,
			})
			if want := body + tc.note; text != want {
				t.Fatalf("got:\n%s\nwant:\n%s", text, want)
			}
			req := lastRequest(t, h, "/api/v4/posts/"+rootID+"/thread")
			if req.Query.Get("direction") != "up" || req.Query.Get("perPage") != strconv.Itoa(tc.limit) {
				t.Errorf("query = %v, want direction=up perPage=%d", req.Query, tc.limit)
			}
		})
	}
}

func TestReadPostsThreadOtherChannel(t *testing.T) {
	h := newHarness(t)
	rootID := postID("root")
	root := testPost(rootID, testutil.TownSquareID, testutil.OtherID, "question", time.Hour)
	servePosts(h, "/posts/{post_id}/thread", threadList(root, false))
	text, isErr := h.callTool(t, "read_posts", map[string]any{"channel": testutil.TestChannel, "thread_root_id": rootID})
	wantErr(t, text, isErr, "thread root "+rootID+" is not in "+testutil.TestChannel)
}

// The thread of a reply id holds the reply itself, whose root_id names the real root.
func TestReadPostsThreadReply(t *testing.T) {
	h := newHarness(t)
	rootID, replyID := postID("root"), postID("r1")
	root := testPost(rootID, testutil.TestChannelID, testutil.OtherID, "question", 2*time.Hour)
	reply := testPost(replyID, testutil.TestChannelID, testutil.MeID, "answer", time.Hour)
	reply.RootId = rootID
	servePosts(h, "/posts/{post_id}/thread", threadList(root, false, reply))
	text, isErr := h.callTool(t, "read_posts", map[string]any{"channel": testutil.TestChannel, "thread_root_id": replyID})
	wantErr(t, text, isErr, "post "+replyID+" is a reply, not a thread root — its thread root id is "+rootID)
}

func TestReadPostsChannelNotFound(t *testing.T) {
	h := newHarness(t)
	text, isErr := h.callTool(t, "read_posts", map[string]any{"channel": "nope"})
	wantErr(t, text, isErr, "channel not found: nope")
}

func TestReadPostsLimitOutOfRange(t *testing.T) {
	h := newHarness(t)
	for limit, want := range map[int]string{0: "/properties/limit: minimum", 201: "/properties/limit: maximum"} {
		text, isErr := h.callTool(t, "read_posts", map[string]any{"channel": testutil.TestChannel, "limit": limit})
		if !isErr || !strings.Contains(text, want) {
			t.Errorf("limit=%d: got %q (isError %v), want %q", limit, text, isErr, want)
		}
	}
	wantNoRequests(t, h)
}

func TestReadPostsAPIError(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodGet, "/channels/{channel_id}/posts", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusForbidden, "x", "no access")
	})
	text, isErr := h.callTool(t, "read_posts", map[string]any{"channel": testutil.TestChannel})
	wantErr(t, text, isErr, "mattermost API 403 /api/v4/channels/"+testutil.TestChannelID+"/posts: no access")
}

func TestGetPost(t *testing.T) {
	h := newHarness(t)
	p := testPost(postID("p1"), testutil.TestChannelID, testutil.OtherID, "hello", time.Hour)
	h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, p)
	})
	text := h.callOK(t, "get_post", map[string]any{"post_id": p.Id})
	if want := "in " + testutil.TestChannel + ":\n**bob** (1h ago): hello (post " + p.Id + ")"; text != want {
		t.Fatalf("got %q, want %q", text, want)
	}
}

func TestGetPostInDM(t *testing.T) {
	h := newHarness(t)
	p := testPost(postID("p1"), testutil.DMID, testutil.OwnerID, "hello", time.Hour)
	h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, p)
	})
	text := h.callOK(t, "get_post", map[string]any{"post_id": p.Id})
	want := "in " + testutil.DMName + " (DM with @" + testutil.OwnerName + "):\n**" + testutil.OwnerName +
		"** (1h ago): hello (post " + p.Id + ")"
	if text != want {
		t.Fatalf("got %q, want %q", text, want)
	}
}

func TestGetPostChannelUnresolved(t *testing.T) {
	h := newHarness(t)
	gone := postID("gonechannel")
	p := testPost(postID("p1"), gone, testutil.OtherID, "hello", time.Hour)
	h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, p)
	})
	text := h.callOK(t, "get_post", map[string]any{"post_id": p.Id})
	if want := "in " + gone + ":\n**bob** (1h ago): hello (post " + p.Id + ")"; text != want {
		t.Fatalf("got %q, want %q", text, want)
	}
}

func TestGetPostNotFound(t *testing.T) {
	h := newHarness(t)
	id := postID("missing")
	h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusNotFound, "app.post.get.app_error", "Unable to get the post.")
	})
	text, isErr := h.callTool(t, "get_post", map[string]any{"post_id": id})
	wantErr(t, text, isErr, "post "+id+" not found — it is deleted, or in a channel this user cannot read")
}

func TestGetPostAPIError(t *testing.T) {
	h := newHarness(t)
	id := postID("p1")
	h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusInternalServerError, "x", "boom")
	})
	text, isErr := h.callTool(t, "get_post", map[string]any{"post_id": id})
	wantErr(t, text, isErr, "mattermost API 500 /api/v4/posts/"+id+": boom")
}

func TestReadToolSchemas(t *testing.T) {
	h := newHarness(t)
	schemas := h.toolSchemas(t)
	cases := []struct {
		tool     string
		required []string
		props    []string
	}{
		{"list_channels", nil, nil},
		{"read_posts", []string{"channel"}, []string{"channel", "thread_root_id", "pinned", "before", "since", "limit", "full"}},
		{"get_post", []string{"post_id"}, []string{"post_id", "full"}},
	}
	for _, tc := range cases {
		checkSchema(t, schemas, tc.tool, tc.required, tc.props)
	}
	limit := schemas["read_posts"].Properties["limit"]
	if limit.Minimum == nil || *limit.Minimum != 1 || limit.Maximum == nil || *limit.Maximum != 200 {
		t.Errorf("read_posts.limit range = %v..%v, want 1..200", limit.Minimum, limit.Maximum)
	}
}

func TestReadPostsLimitMaxAccepted(t *testing.T) {
	h := newHarness(t)
	servePosts(h, "/channels/{channel_id}/posts", postList())
	h.callOK(t, "read_posts", map[string]any{"channel": testutil.TestChannel, "limit": 200})
	req := lastRequest(t, h, "/api/v4/channels/"+testutil.TestChannelID+"/posts")
	if got := req.Query.Get("per_page"); got != "200" {
		t.Errorf("per_page = %q, want 200", got)
	}
}

func TestReadPostsBranchAPIErrors(t *testing.T) {
	rootID := postID("root")
	cases := []struct {
		name, pattern, path string
		args                map[string]any
	}{
		{"thread", "/posts/{post_id}/thread", "/api/v4/posts/" + rootID + "/thread",
			map[string]any{"channel": testutil.TestChannel, "thread_root_id": rootID}},
		{"pinned", "/channels/{channel_id}/pinned", "/api/v4/channels/" + testutil.TestChannelID + "/pinned",
			map[string]any{"channel": testutil.TestChannel, "pinned": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.fake.Handle(http.MethodGet, tc.pattern, func(w http.ResponseWriter, _ *http.Request) {
				testutil.WriteError(w, http.StatusForbidden, "x", "no access")
			})
			text, isErr := h.callTool(t, "read_posts", tc.args)
			wantErr(t, text, isErr, "mattermost API 403 "+tc.path+": no access")
		})
	}
}
