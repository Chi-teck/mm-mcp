package mattermost

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

var postsNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// ago returns epoch ms d before postsNow.
func ago(d time.Duration) int64 { return postsNow.Add(-d).UnixMilli() }

func postList(prev string, posts ...*model.Post) *model.PostList {
	list := model.NewPostList()
	// Server order is newest first; feed it reversed so the formatter's sort is exercised.
	for i := len(posts) - 1; i >= 0; i-- {
		list.AddPost(posts[i])
		list.AddOrder(posts[i].Id)
	}
	list.PrevPostId = prev
	return list
}

func TestFormatPosts(t *testing.T) {
	names := map[string]string{"u1": "alice", "u2": "bob"}
	long := strings.Repeat("x", 501)
	tests := []struct {
		name string
		list *model.PostList
		opts PostsOptions
		want string
	}{
		{
			name: "empty",
			list: postList(""),
			want: "(no posts)",
		},
		{
			name: "nil list",
			list: nil,
			want: "(no posts)",
		},
		{
			name: "only deleted",
			list: postList("", &model.Post{Id: "p1", UserId: "u1", CreateAt: ago(time.Hour), DeleteAt: ago(time.Minute)}),
			want: "(no posts)",
		},
		{
			name: "thread with files and reactions",
			list: postList("",
				&model.Post{Id: "root", UserId: "u1", CreateAt: ago(2 * time.Hour), Message: "message body",
					Metadata: &model.PostMetadata{Reactions: []*model.Reaction{
						{EmojiName: "thumbsup"}, {EmojiName: "eyes"}, {EmojiName: "thumbsup"},
					}}},
				&model.Post{Id: "gone", UserId: "u2", CreateAt: ago(90 * time.Minute), RootId: "root", DeleteAt: 1},
				&model.Post{Id: "r1", UserId: "u2", CreateAt: ago(time.Hour), Message: "reply body", RootId: "root",
					FileIds: []string{"f1"},
					Metadata: &model.PostMetadata{Files: []*model.FileInfo{
						{Id: "f1", Name: "report.pdf", MimeType: "application/pdf", Size: 1258291},
					}}},
			),
			want: "**alice** (2h ago): message body (post root)\n" +
				"  [reactions] :thumbsup: 2 :eyes: 1\n" +
				"  ↳ **bob** (1h ago): reply body (post r1, thread root)\n" +
				"  [file] report.pdf (application/pdf, 1.2 MB, id: f1)",
		},
		{
			name: "reply to a root not in the list, files before reactions",
			list: postList("",
				&model.Post{Id: "r1", UserId: "u2", CreateAt: ago(30 * time.Minute), Message: "late reply", RootId: "elsewhere",
					FileIds: []string{"f1"},
					Metadata: &model.PostMetadata{
						Files:     []*model.FileInfo{{Id: "f1", Name: "big.iso", MimeType: "application/octet-stream", Size: 3 << 30}},
						Reactions: []*model.Reaction{{EmojiName: "eyes"}},
					}},
				&model.Post{Id: "p2", UserId: "u1", CreateAt: ago(10 * time.Minute), Message: "plain",
					Metadata: &model.PostMetadata{}},
			),
			want: "**bob** (30m ago), reply to elsewhere: late reply (post r1)\n" +
				"  [file] big.iso (application/octet-stream, 3.0 GB, id: f1)\n" +
				"  [reactions] :eyes: 1\n" +
				"**alice** (10m ago): plain (post p2)",
		},
		{
			name: "unavailable files, unknown user",
			list: postList("",
				&model.Post{Id: "p1", UserId: "u9", CreateAt: ago(3 * 24 * time.Hour), Message: "see attached",
					FileIds: []string{"f1", "f2", "f3"},
					Metadata: &model.PostMetadata{Files: []*model.FileInfo{
						{Id: "f1", Name: "a.txt", MimeType: "text/plain", Size: 12},
					}}},
				&model.Post{Id: "p2", UserId: "u1", CreateAt: ago(time.Minute), Message: "no meta", FileIds: []string{"f4"}},
			),
			want: "**u9** (3d ago): see attached (post p1)\n" +
				"  [file] a.txt (text/plain, 12 B, id: f1)\n" +
				"  [file] (2 unavailable — server sent no metadata)\n" +
				"**alice** (1m ago): no meta (post p2)\n" +
				"  [file] (1 unavailable — server sent no metadata)",
		},
		{
			name: "truncated",
			list: postList("", &model.Post{Id: "p1", UserId: "u1", CreateAt: ago(time.Second), Message: long}),
			want: "**alice** (just now): " + strings.Repeat("x", 500) +
				"\n**[truncated at 500 chars — pass full=true for the whole message]** (post p1)",
		},
		{
			name: "full",
			list: postList("", &model.Post{Id: "p1", UserId: "u1", CreateAt: ago(8 * 24 * time.Hour), Message: long}),
			opts: PostsOptions{Full: true},
			want: "**alice** (2026-09-17): " + long + " (post p1)",
		},
		{
			name: "paging hint from prev_post_id",
			list: postList("older",
				&model.Post{Id: "p1", UserId: "u1", CreateAt: ago(2 * time.Hour), Message: "one"},
				&model.Post{Id: "p2", UserId: "u2", CreateAt: ago(time.Hour), Message: "two"},
			),
			opts: PostsOptions{Limit: 30},
			want: "**alice** (2h ago): one (post p1)\n**bob** (1h ago): two (post p2)\n" +
				"(2 posts shown — older posts exist, pass before=p1)",
		},
		{
			name: "truncated reply keeps thread suffix after marker",
			list: postList("",
				&model.Post{Id: "root", UserId: "u1", CreateAt: ago(time.Minute), Message: "q"},
				&model.Post{Id: "r1", UserId: "u2", CreateAt: ago(time.Second), Message: long, RootId: "root"},
			),
			want: "**alice** (1m ago): q (post root)\n" +
				"  ↳ **bob** (just now): " + strings.Repeat("x", 500) +
				"\n**[truncated at 500 chars — pass full=true for the whole message]** (post r1, thread root)",
		},
		{
			name: "truncated reply to another thread keeps post suffix after marker",
			list: postList("", &model.Post{Id: "r1", UserId: "u2", CreateAt: ago(time.Second), Message: long, RootId: "root"}),
			want: "**bob** (just now), reply to root: " + strings.Repeat("x", 500) +
				"\n**[truncated at 500 chars — pass full=true for the whole message]** (post r1)",
		},
		{
			name: "reply to another thread is not indented under the root above",
			list: postList("",
				&model.Post{Id: "a", UserId: "u1", CreateAt: ago(4 * time.Hour), Message: "root a"},
				&model.Post{Id: "a1", UserId: "u2", CreateAt: ago(3 * time.Hour), Message: "on a", RootId: "a"},
				&model.Post{Id: "b1", UserId: "u2", CreateAt: ago(2 * time.Hour), Message: "on b", RootId: "b"},
				&model.Post{Id: "b2", UserId: "u1", CreateAt: ago(time.Hour), Message: "on b again", RootId: "b"},
				&model.Post{Id: "a2", UserId: "u1", CreateAt: ago(time.Minute), Message: "back on a", RootId: "a"},
			),
			want: "**alice** (4h ago): root a (post a)\n" +
				"  ↳ **bob** (3h ago): on a (post a1, thread a)\n" +
				"**bob** (2h ago), reply to b: on b (post b1)\n" +
				"  ↳ **alice** (1h ago): on b again (post b2, thread b)\n" +
				"**alice** (1m ago), reply to a: back on a (post a2)",
		},
		{
			name: "reply after a root of another thread",
			list: postList("",
				&model.Post{Id: "a", UserId: "u1", CreateAt: ago(2 * time.Hour), Message: "root a"},
				&model.Post{Id: "b1", UserId: "u2", CreateAt: ago(time.Hour), Message: "on b", RootId: "b"},
			),
			want: "**alice** (2h ago): root a (post a)\n" +
				"**bob** (1h ago), reply to b: on b (post b1)",
		},
		{
			name: "system messages",
			list: postList("",
				&model.Post{Id: "s1", UserId: "u1", CreateAt: ago(2 * time.Hour), Type: model.PostTypeJoinChannel,
					Message: "alice joined the channel."},
				&model.Post{Id: "s2", UserId: "u1", CreateAt: ago(time.Hour), Type: model.PostTypeAddToChannel, RootId: "s1",
					Message: "bob added to the channel by alice."},
				&model.Post{Id: "s3", UserId: "u1", CreateAt: ago(time.Minute), Type: model.PostTypeAddToChannel, RootId: "x",
					Message: "bob added to the channel by alice."},
				&model.Post{Id: "m1", UserId: "u2", CreateAt: ago(time.Second), Type: model.PostTypeMe, Message: "waves"},
			),
			want: "[system] (2h ago): alice joined the channel. (post s1)\n" +
				"  ↳ [system] (1h ago): bob added to the channel by alice. (post s2, thread s1)\n" +
				"[system] (1m ago), reply to x: bob added to the channel by alice. (post s3)\n" +
				"**bob** (just now): waves (post m1)",
		},
		{
			name: "edited posts",
			list: postList("",
				&model.Post{Id: "root", UserId: "u1", CreateAt: ago(2 * time.Hour), EditAt: ago(time.Hour), Message: "fixed"},
				&model.Post{Id: "r1", UserId: "u2", CreateAt: ago(time.Hour), EditAt: ago(time.Minute), Message: "also fixed", RootId: "root"},
				&model.Post{Id: "r2", UserId: "u2", CreateAt: ago(time.Minute), EditAt: ago(time.Second), Message: "other", RootId: "x"},
				&model.Post{Id: "p2", UserId: "u1", CreateAt: ago(time.Second), UpdateAt: ago(0), Message: "reacted to"},
			),
			want: "**alice** (2h ago) (edited): fixed (post root)\n" +
				"  ↳ **bob** (1h ago) (edited): also fixed (post r1, thread root)\n" +
				"**bob** (1m ago) (edited), reply to x: other (post r2)\n" +
				"**alice** (just now): reacted to (post p2)",
		},
		{
			name: "no limit, paging hint from prev_post_id",
			list: postList("older",
				&model.Post{Id: "p1", UserId: "u1", CreateAt: ago(2 * time.Hour), Message: "one"},
				&model.Post{Id: "p2", UserId: "u2", CreateAt: ago(time.Hour), Message: "two"},
			),
			want: "**alice** (2h ago): one (post p1)\n**bob** (1h ago): two (post p2)\n" +
				"(2 posts shown — older posts exist, pass before=p1)",
		},
		{
			name: "trimmed to limit",
			list: postList("",
				&model.Post{Id: "p1", UserId: "u1", CreateAt: ago(3 * time.Hour), Message: "one"},
				&model.Post{Id: "p2", UserId: "u2", CreateAt: ago(2 * time.Hour), Message: "two"},
				&model.Post{Id: "p3", UserId: "u1", CreateAt: ago(time.Hour), Message: "three"},
			),
			opts: PostsOptions{Limit: 2},
			want: "**bob** (2h ago): two (post p2)\n**alice** (1h ago): three (post p3)\n" +
				"(2 posts shown — older posts exist, pass before=p2)",
		},
		{
			name: "trimmed to one post",
			list: postList("",
				&model.Post{Id: "p1", UserId: "u1", CreateAt: ago(2 * time.Hour), Message: "one"},
				&model.Post{Id: "p2", UserId: "u2", CreateAt: ago(time.Hour), Message: "two"},
			),
			opts: PostsOptions{Limit: 1},
			want: "**bob** (1h ago): two (post p2)\n(1 post shown — older posts exist, pass before=p2)",
		},
		{
			name: "deleted posts do not count toward limit",
			list: postList("",
				&model.Post{Id: "p1", UserId: "u1", CreateAt: ago(3 * time.Hour), Message: "one"},
				&model.Post{Id: "p2", UserId: "u2", CreateAt: ago(2 * time.Hour), DeleteAt: ago(time.Hour)},
				&model.Post{Id: "p3", UserId: "u1", CreateAt: ago(time.Hour), Message: "three"},
			),
			opts: PostsOptions{Limit: 2},
			want: "**alice** (3h ago): one (post p1)\n**alice** (1h ago): three (post p3)",
		},
		{
			name: "no paging",
			list: postList("older",
				&model.Post{Id: "p1", UserId: "u1", CreateAt: ago(3 * time.Hour), Message: "one"},
				&model.Post{Id: "p2", UserId: "u2", CreateAt: ago(2 * time.Hour), Message: "two"},
			),
			opts: PostsOptions{Limit: 1, NoPaging: true},
			want: "**bob** (2h ago): two (post p2)",
		},
		{
			name: "no hint at start of channel",
			list: postList("", &model.Post{Id: "p1", UserId: "u1", CreateAt: ago(time.Hour), Message: "one"}),
			opts: PostsOptions{Limit: 1},
			want: "**alice** (1h ago): one (post p1)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatPosts(tt.list, names, postsNow, tt.opts)
			if got != tt.want {
				t.Errorf("got\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestContextFormatPosts(t *testing.T) {
	c, s := newTestContext(t)
	c.now = func() time.Time { return postsNow }
	list := postList("",
		&model.Post{Id: "p1", UserId: testutil.OtherID, CreateAt: ago(3 * time.Hour), Message: "trimmed"},
		&model.Post{Id: "p2", UserId: testutil.MeID, CreateAt: ago(2 * time.Hour), Message: "hi"},
		&model.Post{Id: "p3", UserId: testutil.OwnerID, CreateAt: ago(time.Hour), Message: "hello"},
	)
	got, err := c.FormatPosts(context.Background(), list, PostsOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := "**" + testutil.MeUsername + "** (2h ago): hi (post p2)\n**" + testutil.OwnerName + "** (1h ago): hello (post p3)\n" +
		"(2 posts shown — older posts exist, pass before=p2)"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// Only the authors of shown posts are looked up.
	for _, r := range s.Requests() {
		if r.Path == testutil.APIPrefix+"/users/ids" && strings.Contains(string(r.Body), testutil.OtherID) {
			t.Errorf("looked up the author of a trimmed post: %s", r.Body)
		}
	}
}

func TestContextFormatPostsUsersLookupFails(t *testing.T) {
	c, s := newTestContext(t)
	var log strings.Builder
	old := diagLog
	diagLog = &log
	t.Cleanup(func() { diagLog = old })
	s.Handle("POST", "/users/ids", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusInternalServerError, "x", "boom")
	})
	list := postList("", &model.Post{Id: "p1", UserId: testutil.OtherID, CreateAt: ago(time.Hour), Message: "m"})
	got, err := c.FormatPosts(context.Background(), list, PostsOptions{})
	if want := "**" + testutil.OtherID + "** ("; err != nil || !strings.HasPrefix(got, want) {
		t.Fatalf("got %q, %v; want author by id", got, err)
	}
	if log.Len() == 0 {
		t.Error("lookup failure not logged")
	}
}
