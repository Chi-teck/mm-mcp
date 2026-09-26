package tools

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

// calls lists the requests sent to the fake as "METHOD /path", GETs included when withGets.
func calls(h *harness, withGets bool) []string {
	var out []string
	for _, r := range h.fake.Requests() {
		if withGets || r.Method != http.MethodGet {
			out = append(out, r.Method+" "+r.Path)
		}
	}
	return out
}

// wantCalls fails the test unless the non-GET requests sent are exactly want.
func wantCalls(t *testing.T, h *harness, want ...string) {
	t.Helper()
	if got := calls(h, false); !slices.Equal(got, want) {
		t.Fatalf("write requests:\n got %q\nwant %q", got, want)
	}
}

// bodyOf decodes the JSON body of the last request with method and path into v.
func bodyOf(t *testing.T, h *harness, method, path string, v any) {
	t.Helper()
	reqs := h.fake.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Method == method && reqs[i].Path == path {
			if err := json.Unmarshal(reqs[i].Body, v); err != nil {
				t.Fatalf("%s %s body %q: %v", method, path, reqs[i].Body, err)
			}
			return
		}
	}
	t.Fatalf("no %s %s request; sent %q", method, path, calls(h, true))
}

// serveOK answers method+pattern with 200 {"status":"OK"}.
func serveOK(h *harness, method, pattern string) {
	h.fake.Handle(method, pattern, func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "OK"})
	})
}

// serveFail answers method+pattern with an AppError of status and message.
func serveFail(h *harness, method, pattern string, status int, message string) {
	h.fake.Handle(method, pattern, func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, status, "test.error", message)
	})
}

func followPath() string {
	return testutil.APIPrefix + "/users/" + testutil.MeID + "/teams/" + testutil.TeamID +
		"/threads/" + postID("root") + "/following"
}

// serveThreadRoot answers GET /posts/{id} with a root post that has replies.
func serveThreadRoot(h *harness) {
	serveRoot(h, &model.Post{Id: postID("root"), ChannelId: testutil.TestChannelID, ReplyCount: 2})
}

func TestFollowThread(t *testing.T) {
	h := newHarness(t)
	serveThreadRoot(h)
	serveOK(h, http.MethodPut, "/users/{user_id}/teams/{team_id}/threads/{thread_id}/following")

	got := h.callOK(t, "follow_thread", map[string]any{"thread_root_id": postID("root")})
	if want := "Following thread " + postID("root"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	wantCalls(t, h, "PUT "+followPath())
}

func TestUnfollowThread(t *testing.T) {
	h := newHarness(t)
	serveThreadRoot(h)
	serveOK(h, http.MethodDelete, "/users/{user_id}/teams/{team_id}/threads/{thread_id}/following")

	got := h.callOK(t, "unfollow_thread", map[string]any{"thread_root_id": postID("root")})
	if want := "Left thread " + postID("root") + " — posting in it again re-follows it."; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	wantCalls(t, h, "DELETE "+followPath())
}

func TestFollowThreadErrors(t *testing.T) {
	for _, tc := range []struct{ tool, method string }{
		{"follow_thread", http.MethodPut},
		{"unfollow_thread", http.MethodDelete},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			h := newHarness(t)
			serveThreadRoot(h)
			serveFail(h, tc.method, "/users/{user_id}/teams/{team_id}/threads/{thread_id}/following",
				http.StatusNotFound, "no thread")
			text, isErr := h.callTool(t, tc.tool, map[string]any{"thread_root_id": postID("root")})
			wantErr(t, text, isErr, "mattermost API 404 "+followPath()+": no thread")

			text, isErr = h.callTool(t, tc.tool, nil)
			wantErr(t, text, isErr, `validating "arguments": validating root: required: missing properties: ["thread_root_id"]`)
		})
	}
}

// TestFollowThreadRootChecks covers the posts the server would accept with a 200 but not follow:
// no request past GET /posts/{id} may be sent.
func TestFollowThreadRootChecks(t *testing.T) {
	id := postID("root")
	notFound := func(h *harness) {
		serveFail(h, http.MethodGet, "/posts/{post_id}", http.StatusNotFound, "Unable to get the post.")
	}
	reply := func(h *harness) {
		serveRoot(h, &model.Post{Id: id, ChannelId: testutil.TestChannelID, RootId: postID("real"), ReplyCount: 3})
	}
	noReplies := func(h *harness) { serveRoot(h, &model.Post{Id: id, ChannelId: testutil.TestChannelID}) }
	failed := func(h *harness) {
		serveFail(h, http.MethodGet, "/posts/{post_id}", http.StatusForbidden, "denied")
	}
	notFoundText := "thread root " + id + " not found — it is deleted, or in a channel this user cannot read"
	replyText := "post " + id + " is a reply, not a thread root — its thread root id is " + postID("real")
	failedText := "mattermost API 403 " + testutil.APIPrefix + "/posts/" + id + ": denied"
	tests := []struct {
		name, tool string
		serve      func(h *harness)
		want       string
		isErr      bool
	}{
		{"follow not found", "follow_thread", notFound, notFoundText, true},
		{"unfollow not found", "unfollow_thread", notFound, notFoundText, true},
		{"follow reply", "follow_thread", reply, replyText, true},
		{"unfollow reply", "unfollow_thread", reply, replyText, true},
		{"follow no replies", "follow_thread", noReplies, "post " + id + " has no replies yet, so there is no " +
			"thread to follow. Mattermost follows it automatically for its author and for anyone who replies", true},
		{"unfollow no replies", "unfollow_thread", noReplies,
			"Post " + id + " has no replies yet, so there is no thread to leave — nothing to do.", false},
		{"follow other error", "follow_thread", failed, failedText, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			serveOK(h, http.MethodPut, "/users/{user_id}/teams/{team_id}/threads/{thread_id}/following")
			serveOK(h, http.MethodDelete, "/users/{user_id}/teams/{team_id}/threads/{thread_id}/following")
			tt.serve(h)
			text, isErr := h.callTool(t, tt.tool, map[string]any{"thread_root_id": id})
			if text != tt.want || isErr != tt.isErr {
				t.Fatalf("got %q (isError %v), want %q (isError %v)", text, isErr, tt.want, tt.isErr)
			}
			wantCalls(t, h)
		})
	}
}

func reactionsFor(h *harness, reactions ...*model.Reaction) {
	h.fake.Handle(http.MethodGet, "/posts/{post_id}/reactions", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, reactions)
	})
}

func TestReactAdd(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodPost, "/reactions", func(w http.ResponseWriter, r *http.Request) {
		var in model.Reaction
		_ = json.NewDecoder(r.Body).Decode(&in)
		testutil.WriteJSON(w, http.StatusOK, in)
	})
	p := postID("p1")
	got := h.callOK(t, "react", map[string]any{"post_id": p, "emoji": "thumbsup", "action": "add"})
	if want := "Added :thumbsup: on post " + p; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	wantCalls(t, h, "POST "+testutil.APIPrefix+"/reactions")
	var sent model.Reaction
	bodyOf(t, h, http.MethodPost, testutil.APIPrefix+"/reactions", &sent)
	if sent.UserId != testutil.MeID || sent.PostId != p || sent.EmojiName != "thumbsup" {
		t.Fatalf("reaction sent: %+v", sent)
	}
}

func TestReactAddFails(t *testing.T) {
	h := newHarness(t)
	serveFail(h, http.MethodPost, "/reactions", http.StatusBadRequest, "bad emoji")
	text, isErr := h.callTool(t, "react", map[string]any{"post_id": postID("p1"), "emoji": "nope", "action": "add"})
	wantErr(t, text, isErr, "mattermost API 400 "+testutil.APIPrefix+"/reactions: bad emoji")
}

func TestReactRemove(t *testing.T) {
	h := newHarness(t)
	p := postID("p1")
	reactionsFor(h,
		&model.Reaction{UserId: testutil.OtherID, PostId: p, EmojiName: "eyes"},
		&model.Reaction{UserId: testutil.MeID, PostId: p, EmojiName: "eyes"})
	serveOK(h, http.MethodDelete, "/users/{user_id}/posts/{post_id}/reactions/{emoji_name}")

	got := h.callOK(t, "react", map[string]any{"post_id": p, "emoji": "eyes", "action": "remove"})
	if want := "Removed :eyes: from post " + p; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	wantCalls(t, h, "DELETE "+testutil.APIPrefix+"/users/"+testutil.MeID+"/posts/"+p+"/reactions/eyes")
	if !slices.Contains(calls(h, true), "GET "+testutil.APIPrefix+"/posts/"+p+"/reactions") {
		t.Fatalf("reactions not checked first: %q", calls(h, true))
	}
}

func TestReactRemoveMissing(t *testing.T) {
	h := newHarness(t)
	p := postID("p1")
	// Someone else's :eyes: and my own :tada: do not count as my :eyes:.
	reactionsFor(h,
		&model.Reaction{UserId: testutil.OtherID, PostId: p, EmojiName: "eyes"},
		&model.Reaction{UserId: testutil.MeID, PostId: p, EmojiName: "tada"})

	got := h.callOK(t, "react", map[string]any{"post_id": p, "emoji": "eyes", "action": "remove"})
	if want := "No :eyes: reaction by you on post " + p + " — nothing to remove."; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	wantCalls(t, h)
}

func TestReactRemoveErrors(t *testing.T) {
	p := postID("p1")
	t.Run("check fails", func(t *testing.T) {
		h := newHarness(t)
		serveFail(h, http.MethodGet, "/posts/{post_id}/reactions", http.StatusForbidden, "no access")
		text, isErr := h.callTool(t, "react", map[string]any{"post_id": p, "emoji": "eyes", "action": "remove"})
		wantErr(t, text, isErr, "mattermost API 403 "+testutil.APIPrefix+"/posts/"+p+"/reactions: no access")
		wantCalls(t, h)
	})
	t.Run("delete fails", func(t *testing.T) {
		h := newHarness(t)
		reactionsFor(h, &model.Reaction{UserId: testutil.MeID, PostId: p, EmojiName: "eyes"})
		serveFail(h, http.MethodDelete, "/users/{user_id}/posts/{post_id}/reactions/{emoji_name}",
			http.StatusForbidden, "denied")
		text, isErr := h.callTool(t, "react", map[string]any{"post_id": p, "emoji": "eyes", "action": "remove"})
		wantErr(t, text, isErr,
			"mattermost API 403 "+testutil.APIPrefix+"/users/"+testutil.MeID+"/posts/"+p+"/reactions/eyes: denied")
	})
	t.Run("delete fails, path escaped", func(t *testing.T) {
		h := newHarness(t)
		reactionsFor(h, &model.Reaction{UserId: testutil.MeID, PostId: p, EmojiName: "a b"})
		serveFail(h, http.MethodDelete, "/users/{user_id}/posts/{post_id}/reactions/{emoji_name}",
			http.StatusBadRequest, "bad emoji")
		text, isErr := h.callTool(t, "react", map[string]any{"post_id": p, "emoji": "a b", "action": "remove"})
		wantErr(t, text, isErr,
			"mattermost API 400 "+testutil.APIPrefix+"/users/"+testutil.MeID+"/posts/"+p+"/reactions/a%20b: bad emoji")
	})
}

// A ":ThumbsUp:"-style argument is normalized before the request, the comparison and the output.
func TestReactNormalizesEmoji(t *testing.T) {
	p := postID("p1")
	t.Run("add", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Handle(http.MethodPost, "/reactions", func(w http.ResponseWriter, r *http.Request) {
			var in model.Reaction
			_ = json.NewDecoder(r.Body).Decode(&in)
			testutil.WriteJSON(w, http.StatusOK, in)
		})
		got := h.callOK(t, "react", map[string]any{"post_id": p, "emoji": ":ThumbsUp:", "action": "add"})
		if want := "Added :thumbsup: on post " + p; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		var sent model.Reaction
		bodyOf(t, h, http.MethodPost, testutil.APIPrefix+"/reactions", &sent)
		if sent.EmojiName != "thumbsup" {
			t.Fatalf("emoji sent: %q", sent.EmojiName)
		}
	})
	t.Run("remove", func(t *testing.T) {
		h := newHarness(t)
		reactionsFor(h, &model.Reaction{UserId: testutil.MeID, PostId: p, EmojiName: "thumbsup"})
		serveOK(h, http.MethodDelete, "/users/{user_id}/posts/{post_id}/reactions/{emoji_name}")
		got := h.callOK(t, "react", map[string]any{"post_id": p, "emoji": ":ThumbsUp:", "action": "remove"})
		if want := "Removed :thumbsup: from post " + p; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		wantCalls(t, h, "DELETE "+testutil.APIPrefix+"/users/"+testutil.MeID+"/posts/"+p+"/reactions/thumbsup")
	})
	t.Run("remove missing", func(t *testing.T) {
		h := newHarness(t)
		reactionsFor(h)
		got := h.callOK(t, "react", map[string]any{"post_id": p, "emoji": ":Eyes:", "action": "remove"})
		if want := "No :eyes: reaction by you on post " + p + " — nothing to remove."; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestReactBadAction(t *testing.T) {
	h := newHarness(t)
	text, isErr := h.callTool(t, "react", map[string]any{"post_id": postID("p1"), "emoji": "eyes", "action": "toggle"})
	wantErr(t, text, isErr, `validating "arguments": validating root: validating /properties/action: `+
		`enum: toggle does not equal any of: [add remove]`)
	if got := calls(h, true); len(got) != 0 {
		t.Fatalf("requests sent: %q", got)
	}
}

// serveOwnPost answers GET /posts/{id} with a post by the current user.
func serveOwnPost(h *harness) {
	h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, r *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, testPost(r.PathValue("post_id"), testutil.TestChannelID, testutil.MeID, "old", 0))
	})
}

func TestEditPost(t *testing.T) {
	h := newHarness(t)
	serveOwnPost(h)
	p := postID("p1")
	h.fake.Handle(http.MethodPut, "/posts/{post_id}/patch", func(w http.ResponseWriter, r *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, testPost(r.PathValue("post_id"), testutil.TestChannelID, testutil.MeID, "new", 0))
	})
	got := h.callOK(t, "edit_post", map[string]any{"post_id": p, "action": "edit", "message": "new *text*"})
	if want := "Edited post " + p; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	path := testutil.APIPrefix + "/posts/" + p + "/patch"
	wantCalls(t, h, "PUT "+path)
	var patch model.PostPatch
	bodyOf(t, h, http.MethodPut, path, &patch)
	if patch.Message == nil || *patch.Message != "new *text*" || patch.FileIds != nil || patch.Props != nil {
		t.Fatalf("patch sent: %+v", patch)
	}
}

func TestEditPostNeedsMessage(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"absent": {"post_id": postID("p1"), "action": "edit"},
		"empty":  {"post_id": postID("p1"), "action": "edit", "message": ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			text, isErr := h.callTool(t, "edit_post", args)
			wantErr(t, text, isErr, "edit requires a message")
			if got := calls(h, true); len(got) != 0 {
				t.Fatalf("requests sent: %q", got)
			}
		})
	}
}

func TestEditPostEditFails(t *testing.T) {
	h := newHarness(t)
	serveOwnPost(h)
	p := postID("p1")
	serveFail(h, http.MethodPut, "/posts/{post_id}/patch", http.StatusForbidden, "not yours")
	text, isErr := h.callTool(t, "edit_post", map[string]any{"post_id": p, "action": "edit", "message": "x"})
	wantErr(t, text, isErr, "mattermost API 403 "+testutil.APIPrefix+"/posts/"+p+"/patch: not yours")
}

func TestEditPostDelete(t *testing.T) {
	p := postID("p1")
	path := testutil.APIPrefix + "/posts/" + p
	cases := []struct {
		name    string
		status  int
		want    string
		isError bool
	}{
		{"deleted", http.StatusOK, "Deleted post " + p, false},
		{"missing", http.StatusNotFound, "Post " + p + " is already deleted or does not exist — nothing to do.", false},
		{"forbidden", http.StatusForbidden, "mattermost API 403 " + path + ": not yours", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			serveOwnPost(h)
			h.fake.Handle(http.MethodDelete, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
				if tc.status == http.StatusOK {
					testutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "OK"})
					return
				}
				testutil.WriteError(w, tc.status, "test.error", "not yours")
			})
			// A message passed with delete is ignored.
			text, isErr := h.callTool(t, "edit_post", map[string]any{"post_id": p, "action": "delete", "message": "x"})
			if text != tc.want || isErr != tc.isError {
				t.Fatalf("got %q (isError %v), want %q (isError %v)", text, isErr, tc.want, tc.isError)
			}
			wantCalls(t, h, "DELETE "+path)
		})
	}
}

// TestEditPostChecks covers the refusals decided by GET /posts/{id}: no write may be sent.
func TestEditPostChecks(t *testing.T) {
	p := postID("p1")
	notFound := func(h *harness) {
		serveFail(h, http.MethodGet, "/posts/{post_id}", http.StatusNotFound, "Unable to get the post.")
	}
	others := func(h *harness) {
		h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
			testutil.WriteJSON(w, http.StatusOK, testPost(p, testutil.TestChannelID, testutil.OwnerID, "theirs", 0))
		})
	}
	failed := func(h *harness) {
		serveFail(h, http.MethodGet, "/posts/{post_id}", http.StatusForbidden, "denied")
	}
	othersText := "post " + p + " was written by @" + testutil.OwnerName + ", not by you — edit_post only changes your own posts"
	failedText := "mattermost API 403 " + testutil.APIPrefix + "/posts/" + p + ": denied"
	tests := []struct {
		name, action string
		serve        func(*harness)
		want         string
		isError      bool
	}{
		{"edit missing", "edit", notFound,
			"post " + p + " not found — it is deleted, or in a channel this user cannot read", true},
		{"delete missing", "delete", notFound, "Post " + p + " is already deleted or does not exist — nothing to do.", false},
		{"edit others", "edit", others, othersText, true},
		{"delete others", "delete", others, othersText, true},
		{"edit lookup fails", "edit", failed, failedText, true},
		{"delete lookup fails", "delete", failed, failedText, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.serve(h)
			text, isErr := h.callTool(t, "edit_post", map[string]any{"post_id": p, "action": tc.action, "message": "x"})
			if text != tc.want || isErr != tc.isError {
				t.Fatalf("got %q (isError %v), want %q (isError %v)", text, isErr, tc.want, tc.isError)
			}
			for _, call := range calls(h, false) {
				// POST /users/ids is the author lookup, a read.
				if call != "POST "+testutil.APIPrefix+"/users/ids" {
					t.Fatalf("write sent: %s", call)
				}
			}
		})
	}
}

func TestEditPostBadAction(t *testing.T) {
	h := newHarness(t)
	text, isErr := h.callTool(t, "edit_post", map[string]any{"post_id": postID("p1"), "action": "pin"})
	wantErr(t, text, isErr, `validating "arguments": validating root: validating /properties/action: `+
		`enum: pin does not equal any of: [edit delete]`)
	if got := calls(h, true); len(got) != 0 {
		t.Fatalf("requests sent: %q", got)
	}
}

func TestDM(t *testing.T) {
	h := newHarness(t)
	h.fake.Handle(http.MethodPost, "/channels/direct", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusCreated, fakeChannel(t, h, testutil.DMID))
	})
	h.fake.Handle(http.MethodGet, "/users/username/{username}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, &model.User{Id: testutil.OwnerID, Username: testutil.OwnerName})
	})
	got := h.callOK(t, "dm", map[string]any{"username": "Ivan.CH"}) // the reply names the server's username
	want := "DM channel with @ivan.ch: " + testutil.DMName + " (id: " + testutil.DMID +
		") — pass it as the channel to create_post"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	path := testutil.APIPrefix + "/channels/direct"
	wantCalls(t, h, "POST "+path)
	var ids []string
	bodyOf(t, h, http.MethodPost, path, &ids)
	if !slices.Equal(ids, []string{testutil.MeID, testutil.OwnerID}) {
		t.Fatalf("direct channel members: %q", ids)
	}
}

func TestDMErrors(t *testing.T) {
	t.Run("unknown user", func(t *testing.T) {
		h := newHarness(t)
		text, isErr := h.callTool(t, "dm", map[string]any{"username": "nobody"})
		if !isErr || !strings.HasPrefix(text, "mattermost API 404 "+testutil.APIPrefix+"/users/username/nobody: ") {
			t.Fatalf("got %q (isError %v)", text, isErr)
		}
		wantCalls(t, h)
	})
	t.Run("unknown user with a space", func(t *testing.T) {
		h := newHarness(t)
		text, isErr := h.callTool(t, "dm", map[string]any{"username": "no body"})
		if !isErr || !strings.HasPrefix(text, "mattermost API 404 "+testutil.APIPrefix+"/users/username/no%20body: ") {
			t.Fatalf("got %q (isError %v)", text, isErr)
		}
	})
	t.Run("dots refused", func(t *testing.T) { // Client4 would strip ".." and look up ivanch
		h := newHarness(t)
		text, isErr := h.callTool(t, "dm", map[string]any{"username": "ivan..ch"})
		wantErr(t, text, isErr, `invalid username: "ivan..ch" (".." is not allowed)`)
		for _, r := range h.fake.Requests() {
			if strings.Contains(r.Path, "/users/username/") {
				t.Fatalf("request sent: %s %s", r.Method, r.Path)
			}
		}
	})
	t.Run("server returns another user", func(t *testing.T) {
		h := newHarness(t)
		h.fake.Handle(http.MethodGet, "/users/username/{username}", func(w http.ResponseWriter, _ *http.Request) {
			testutil.WriteJSON(w, http.StatusOK, &model.User{Id: testutil.OwnerID, Username: "ivanch"})
		})
		text, isErr := h.callTool(t, "dm", map[string]any{"username": testutil.OwnerName})
		wantErr(t, text, isErr, `mattermost returned @ivanch for username "ivan.ch"; refusing to open the DM`)
		wantCalls(t, h)
	})
	t.Run("create fails", func(t *testing.T) {
		h := newHarness(t)
		serveFail(h, http.MethodPost, "/channels/direct", http.StatusForbidden, "no DMs")
		text, isErr := h.callTool(t, "dm", map[string]any{"username": testutil.OwnerName})
		wantErr(t, text, isErr, "mattermost API 403 "+testutil.APIPrefix+"/channels/direct: no DMs")
	})
}

func TestMiscToolSchemas(t *testing.T) {
	h := newHarness(t)
	schemas := h.toolSchemas(t)
	cases := []struct {
		tool     string
		required []string
		props    []string
		enum     []any
	}{
		{"follow_thread", []string{"thread_root_id"}, []string{"thread_root_id"}, nil},
		{"unfollow_thread", []string{"thread_root_id"}, []string{"thread_root_id"}, nil},
		{"react", []string{"post_id", "emoji", "action"}, []string{"post_id", "emoji", "action"}, []any{"add", "remove"}},
		{"edit_post", []string{"post_id", "action"}, []string{"post_id", "action", "message"}, []any{"edit", "delete"}},
		{"dm", []string{"username"}, []string{"username"}, nil},
	}
	for _, tc := range cases {
		checkSchema(t, schemas, tc.tool, tc.required, tc.props)
		if tc.enum != nil {
			if got := schemas[tc.tool].Properties["action"].Enum; !slices.Equal(got, tc.enum) {
				t.Errorf("%s.action enum = %v, want %v", tc.tool, got, tc.enum)
			}
		}
	}
}

// TestInvalidIDsRefused checks that id arguments that become path segments are refused before
// any request: "." survives Client4's segment cleaning and would shift the request path.
func TestInvalidIDsRefused(t *testing.T) {
	tests := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_post", map[string]any{"post_id": "."}, `invalid post id: "." (expected a 26-char id)`},
		{"react", map[string]any{"post_id": "...", "emoji": "eyes", "action": "remove"},
			`invalid post id: "..." (expected a 26-char id)`},
		{"edit_post", map[string]any{"post_id": ".", "action": "edit", "message": "x"},
			`invalid post id: "." (expected a 26-char id)`},
		{"edit_post", map[string]any{"post_id": "x/../../users/me", "action": "delete"},
			`invalid post id: "x/../../users/me" (expected a 26-char id)`},
		{"follow_thread", map[string]any{"thread_root_id": "."}, `invalid thread root id: "." (expected a 26-char id)`},
		{"unfollow_thread", map[string]any{"thread_root_id": "a?b"}, `invalid thread root id: "a?b" (expected a 26-char id)`},
		{"read_posts", map[string]any{"channel": testutil.TestChannel, "thread_root_id": "."},
			`invalid thread root id: "." (expected a 26-char id)`},
		{"create_post", map[string]any{"channel": "nope", "message": "x", "thread_root_id": "not-an-id", "attachments": []string{"a.txt"}},
			`invalid thread root id: "not-an-id" (expected a 26-char id)`},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			h := newHarness(t)
			text, isErr := h.callTool(t, tt.tool, tt.args)
			wantErr(t, text, isErr, tt.want)
			wantNoRequests(t, h)
		})
	}
}
