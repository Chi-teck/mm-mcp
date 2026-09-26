package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

// fakeChannel returns the fixture channel with id.
func fakeChannel(t *testing.T, h *harness, id string) *model.Channel {
	t.Helper()
	for _, ch := range h.fake.Channels {
		if ch.Id == id {
			return ch
		}
	}
	t.Fatalf("no fixture channel %s", id)
	return nil
}

func TestSearchPosts(t *testing.T) {
	h := newHarness(t)
	list := model.NewPostList()
	// 32 hits in scrambled order: the newest 30 are shown, newest first.
	for i := range 32 {
		p := testPost(postID(fmt.Sprintf("s%02d", i)), testutil.TestChannelID, testutil.OtherID,
			fmt.Sprintf("hit %d", i), time.Duration((i*7)%32+1)*time.Hour)
		list.AddPost(p)
		list.AddOrder(p.Id)
	}
	// Team search also returns DM hits.
	list.Posts[postID("s01")].ChannelId = testutil.DMID
	long := testPost(postID("long"), "unknownchannel000000000000", "unknownuser000000000000000", strings.Repeat("y", 501), time.Minute)
	list.AddPost(long)
	list.AddOrder(long.Id)
	gone := testPost(postID("gone"), testutil.TestChannelID, testutil.OtherID, "deleted", 0)
	gone.DeleteAt = gone.CreateAt
	list.AddPost(gone)
	list.AddOrder(gone.Id)
	h.fake.Handle(http.MethodPost, "/teams/{team_id}/posts/search", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, list)
	})

	text := h.callOK(t, "search", map[string]any{"query": "hit", "type": "posts"})
	lines := strings.Split(text, "\n")
	wantFirst := "**unknownuser000000000000000** (1m ago) in unknownchannel000000000000: " + strings.Repeat("y", 500)
	if lines[0] != wantFirst {
		t.Fatalf("first line:\n got %q\nwant %q", lines[0], wantFirst)
	}
	wantMarker := "**[truncated at 500 chars — read it with read_posts full=true]** (post " + long.Id + ")"
	if lines[1] != wantMarker {
		t.Fatalf("marker line:\n got %q\nwant %q", lines[1], wantMarker)
	}
	hits := lines[2:]
	if len(hits) != 29 {
		t.Fatalf("got %d hit lines after the long one, want 29:\n%s", len(hits), text)
	}
	// Ages are (i*7)%32+1 hours; the newest is i=0 (1h), then i=23 (2h).
	want0 := "**bob** (1h ago) in " + testutil.TestChannel + ": hit 0 (post " + postID("s00") + ")"
	want1 := "**bob** (2h ago) in " + testutil.TestChannel + ": hit 23 (post " + postID("s23") + ")"
	if hits[0] != want0 || hits[1] != want1 {
		t.Fatalf("order:\n%s\n%s\nwant\n%s\n%s", hits[0], hits[1], want0, want1)
	}
	if strings.Contains(text, "deleted") {
		t.Errorf("deleted post shown:\n%s", text)
	}
	if !strings.Contains(text, " in "+testutil.DMName+" (DM with @"+testutil.OwnerName+"): hit 1 ") {
		t.Errorf("DM hit not named:\n%s", text)
	}
	// Known channels come from the one listing of the user's team channels, not per-id GETs.
	gets := calls(h, true)
	listing := "GET " + testutil.APIPrefix + "/users/" + testutil.MeID + "/teams/" + testutil.TeamID + "/channels"
	if i := slices.Index(gets, listing); i < 0 || !slices.Contains(gets[i:], "GET "+testutil.APIPrefix+"/channels/unknownchannel000000000000") {
		t.Errorf("requests %q: want the channel listing, then the per-id fallback for the unknown channel", gets)
	}
	for _, id := range []string{testutil.TestChannelID, testutil.DMID} {
		if slices.Contains(gets, "GET "+testutil.APIPrefix+"/channels/"+id) {
			t.Errorf("channel %s fetched by id despite the listing: %q", id, gets)
		}
	}
	req := lastRequest(t, h, "/api/v4/teams/"+testutil.TeamID+"/posts/search")
	var body model.SearchParameter
	if err := json.Unmarshal(req.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Terms == nil || *body.Terms != "hit" || body.IsOrSearch == nil || *body.IsOrSearch {
		t.Errorf("search body = %s, want terms=hit is_or_search=false", req.Body)
	}
}

func TestSearchPostsNone(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodPost, "/teams/{team_id}/posts/search", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, model.NewPostList())
	})
	if text := h.callOK(t, "search", map[string]any{"query": `a "b"`, "type": "posts"}); text != `No posts found for "a "b"".` {
		t.Fatalf("got %q", text)
	}
}

func TestSearchFiles(t *testing.T) {
	h := newHarness(t)
	list := &model.FileInfoList{FileInfos: map[string]*model.FileInfo{}}
	for i := range 31 {
		id := postID(fmt.Sprintf("f%02d", i))
		list.Order = append(list.Order, id)
		list.FileInfos[id] = &model.FileInfo{Id: id, Name: fmt.Sprintf("r%d.pdf", i), MimeType: "application/pdf",
			Size: 1258291, ChannelId: testutil.TestChannelID}
	}
	list.FileInfos[postID("f01")].ChannelId = "unknownchannel000000000000"
	h.fake.Handle(http.MethodPost, "/teams/{team_id}/files/search", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, list)
	})
	text := h.callOK(t, "search", map[string]any{"query": "report", "type": "files"})
	lines := strings.Split(text, "\n")
	if len(lines) != 30 {
		t.Fatalf("got %d lines, want 30", len(lines))
	}
	want := "[file] r0.pdf (application/pdf, 1.2 MB, id: " + postID("f00") + ") [" + testutil.TestChannel + "]"
	if lines[0] != want {
		t.Fatalf("got %q\nwant %q", lines[0], want)
	}
	if want := "[unknownchannel000000000000]"; !strings.HasSuffix(lines[1], want) {
		t.Errorf("unresolved channel: got %q, want suffix %q", lines[1], want)
	}
	req := lastRequest(t, h, "/api/v4/teams/"+testutil.TeamID+"/files/search")
	if !strings.Contains(string(req.Body), `"is_or_search":false`) {
		t.Errorf("body = %s, want is_or_search false", req.Body)
	}
}

func TestSearchFilesNone(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodPost, "/teams/{team_id}/files/search", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, &model.FileInfoList{})
	})
	if text := h.callOK(t, "search", map[string]any{"query": "x", "type": "files"}); text != `No files found for "x".` {
		t.Fatalf("got %q", text)
	}
}

func TestSearchBadType(t *testing.T) {
	h := newHarness(t)
	text, isErr := h.callTool(t, "search", map[string]any{"query": "x", "type": "users"})
	if want := "/properties/type: enum: users does not equal"; !isErr || !strings.Contains(text, want) {
		t.Errorf("type=users: got %q (isError %v), want %q", text, isErr, want)
	}
	text, isErr = h.callTool(t, "search", map[string]any{"type": "posts"})
	if want := `required: missing properties: ["query"]`; !isErr || !strings.Contains(text, want) {
		t.Errorf("missing query: got %q (isError %v), want %q", text, isErr, want)
	}
	wantNoRequests(t, h)
}

func TestSearchAPIError(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodPost, "/teams/{team_id}/posts/search", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusBadRequest, "api.post.search", "bad terms")
	})
	text, isErr := h.callTool(t, "search", map[string]any{"query": "x", "type": "posts"})
	wantErr(t, text, isErr, "mattermost API 400 /api/v4/teams/"+testutil.TeamID+"/posts/search: bad terms")
}

// serveMembers answers GET /users?in_channel= with total generated users, paged.
func serveMembers(h *harness, total int) {
	h.fake.Handle(http.MethodGet, "/users", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		users := make([]*model.User, 0)
		for i := page * per; i < total && i < (page+1)*per; i++ {
			users = append(users, &model.User{Id: fmt.Sprintf("u%025d", i), Username: fmt.Sprintf("user%d", i)})
		}
		testutil.WriteJSON(w, http.StatusOK, users)
	})
}

func TestListMembers(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodGet, "/users", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, []*model.User{
			{Id: testutil.OwnerID, Username: "ivan.ch", FirstName: "Ivan", LastName: "Ch"},
			{Id: testutil.OtherID, Username: "bob", FirstName: "Bob"},
			{Id: testutil.MeID, Username: "alice"},
		})
	})
	text := h.callOK(t, "list_members", map[string]any{"channel": testutil.TestChannel})
	want := "- ivan.ch — Ivan Ch\n- bob — Bob\n- alice"
	if text != want {
		t.Fatalf("got:\n%s\nwant:\n%s", text, want)
	}
	req := lastRequest(t, h, "/api/v4/users")
	if req.Query.Get("in_channel") != testutil.TestChannelID || req.Query.Get("per_page") != "200" {
		t.Errorf("query = %v", req.Query)
	}
}

func TestListMembersEmpty(t *testing.T) {
	h := newHarness(t)
	serveMembers(h, 0)
	if text := h.callOK(t, "list_members", map[string]any{"channel": testutil.TestChannel}); text != "No members found." {
		t.Fatalf("got %q", text)
	}
}

func TestListMembersCap(t *testing.T) {
	for _, tc := range []struct {
		total  int
		marker bool
	}{{999, false}, {1000, false}, {1001, true}} {
		t.Run(strconv.Itoa(tc.total), func(t *testing.T) {
			h := newHarness(t)
			serveMembers(h, tc.total)
			text := h.callOK(t, "list_members", map[string]any{"channel": testutil.TestChannel})
			lines := strings.Split(text, "\n")
			marker := "(first 1000 members — pass query to search)"
			wantLines := min(tc.total, 1000)
			if tc.marker {
				wantLines++
				if lines[len(lines)-1] != marker {
					t.Fatalf("last line = %q, want %q", lines[len(lines)-1], marker)
				}
			} else if strings.Contains(text, marker) {
				t.Fatalf("unexpected cap marker")
			}
			if len(lines) != wantLines {
				t.Fatalf("got %d lines, want %d", len(lines), wantLines)
			}
			// Five 200-user pages, then at the cap a one-user probe for member 1001.
			var reqs []testutil.Request
			for _, r := range h.fake.Requests() {
				if r.Path == testutil.APIPrefix+"/users" {
					reqs = append(reqs, r)
				}
			}
			wantReqs := 5
			if tc.total >= 1000 {
				wantReqs = 6
				if q := reqs[len(reqs)-1].Query; q.Get("page") != "1000" || q.Get("per_page") != "1" {
					t.Fatalf("probe query = %v, want page=1000 per_page=1", q)
				}
			}
			if len(reqs) != wantReqs {
				t.Fatalf("got %d member requests, want %d", len(reqs), wantReqs)
			}
		})
	}
}

func TestListMembersQuery(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodGet, "/users/autocomplete", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, model.UserAutocomplete{
			Users:        []*model.User{{Id: testutil.OwnerID, Username: "ivan.ch", FirstName: "Ivan", LastName: "Ch"}},
			OutOfChannel: []*model.User{{Id: testutil.OtherID, Username: "ivana"}},
		})
	})
	text := h.callOK(t, "list_members", map[string]any{"channel": testutil.TestChannel, "query": "iva"})
	want := "- ivan.ch — Ivan Ch\n- ivana [NOT in channel — mentions won't notify]"
	if text != want {
		t.Fatalf("got:\n%s\nwant:\n%s", text, want)
	}
	req := lastRequest(t, h, "/api/v4/users/autocomplete")
	q := req.Query
	if q.Get("name") != "iva" || q.Get("in_team") != testutil.TeamID || q.Get("in_channel") != testutil.TestChannelID {
		t.Errorf("query = %v", q)
	}
	if q.Has("limit") || q.Has("page") {
		t.Errorf("query = %v, want the server's default limit and no paging", q)
	}
}

func TestListMembersQueryNone(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodGet, "/users/autocomplete", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, model.UserAutocomplete{Users: []*model.User{}})
	})
	text := h.callOK(t, "list_members", map[string]any{"channel": testutil.TestChannel, "query": "zz"})
	if text != `No users matching "zz".` {
		t.Fatalf("got %q", text)
	}
}

func TestListMembersQueryAPIError(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodGet, "/users/autocomplete", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusForbidden, "api.context.permissions.app_error", "no permission")
	})
	text, isErr := h.callTool(t, "list_members", map[string]any{"channel": testutil.TestChannel, "query": "zz"})
	wantErr(t, text, isErr, "mattermost API 403 /api/v4/users/autocomplete: no permission")
}

func TestListMembersChannelNotFound(t *testing.T) {
	h := newHarness(t)
	text, isErr := h.callTool(t, "list_members", map[string]any{"channel": "nope"})
	wantErr(t, text, isErr, "channel not found: nope")
}

func TestSearchFilesAPIError(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodPost, "/teams/{team_id}/files/search", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusNotImplemented, "api.file.search", "file search disabled")
	})
	text, isErr := h.callTool(t, "search", map[string]any{"query": "x", "type": "files"})
	wantErr(t, text, isErr, "mattermost API 501 /api/v4/teams/"+testutil.TeamID+"/files/search: file search disabled")
}

func TestListMembersAPIError(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodGet, "/users", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusForbidden, "api.context.permissions.app_error", "no permission")
	})
	text, isErr := h.callTool(t, "list_members", map[string]any{"channel": testutil.TestChannel})
	wantErr(t, text, isErr, "mattermost API 403 /api/v4/users: no permission")
}
