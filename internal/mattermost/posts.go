package mattermost

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// NoPosts is the output for a post list with nothing to show.
const NoPosts = "(no posts)"

// PostsOptions controls FormatPosts.
type PostsOptions struct {
	// Limit keeps only the newest Limit posts; 0 or below shows all of them.
	Limit int
	// Full prints bodies whole instead of cutting them at MaxBodyChars.
	Full bool
	// NoPaging suppresses the `pass before=<id>` hint, for callers whose list
	// is not a channel page (get_post) or cannot page with `before` (pinned
	// read_posts).
	NoPaging bool
}

// LivePosts returns the posts of list in list.Order, without deleted posts
// and tombstones. A nil list has none.
func LivePosts(list *model.PostList) []*model.Post {
	if list == nil {
		return nil
	}
	var live []*model.Post
	for _, id := range list.Order {
		post := list.Posts[id]
		// A post list can carry tombstones for deleted posts: delete_at is set
		// and the body blanked. They are not for reading.
		if post == nil || post.DeleteAt != 0 {
			continue
		}
		live = append(live, post)
	}
	return live
}

// AuthorName is the username of userID in names, or userID itself when the
// lookup did not resolve it.
func AuthorName(names map[string]string, userID string) string {
	if who := names[userID]; who != "" {
		return who
	}
	return userID
}

// ShownPosts returns the posts FormatPosts renders: deleted posts and
// tombstones dropped, oldest first, trimmed to the newest limit (0 or below
// keeps all). The second result is the number of posts before trimming.
func ShownPosts(list *model.PostList, limit int) (shown []*model.Post, total int) {
	all := LivePosts(list)
	slices.SortStableFunc(all, func(a, b *model.Post) int {
		return cmp.Compare(a.CreateAt, b.CreateAt)
	})
	if limit > 0 && len(all) > limit {
		return all[len(all)-limit:], len(all)
	}
	return all, len(all)
}

// FormatPosts renders list: one line per post, oldest
// first, ending in `(post <id>)`, replies as
// `  ↳ … (post <id>, thread <root_id>)` when the line above is of the same
// thread and as `…, reply to <root_id>: …` otherwise; system messages carry
// `[system]` in place of the author and edited posts `(edited)` after the
// time. Each post is followed by its `[file]`, unavailable-file and
// `[reactions]` lines. The list ends in the paging hint when the server
// reports older posts (prev_post_id) or the list was trimmed to opts.Limit. Usernames
// come from names, by user id; a missing id is shown as is. An empty result is NoPosts.
func FormatPosts(list *model.PostList, names map[string]string, now time.Time, opts PostsOptions) string {
	shown, total := ShownPosts(list, opts.Limit)
	if len(shown) == 0 {
		return NoPosts
	}
	var lines []string
	// prevThread is the thread of the post on the line above: a reply is
	// indented under it only when it belongs to the same thread.
	prevThread := ""
	for _, post := range shown {
		who := "**" + AuthorName(names, post.UserId) + "**"
		if post.IsSystemMessage() {
			who = "[system]"
		}
		when := "(" + RelTime(post.CreateAt, now) + ")"
		if post.EditAt != 0 {
			when += " (edited)"
		}
		body := post.Message
		if !opts.Full {
			body = Truncate(body, FullHint, MaxBodyChars)
		}
		switch post.RootId {
		case "":
			lines = append(lines, fmt.Sprintf("%s %s: %s (post %s)", who, when, body, post.Id))
			prevThread = post.Id
		case prevThread:
			lines = append(lines, fmt.Sprintf("  ↳ %s %s: %s (post %s, thread %s)", who, when, body, post.Id, post.RootId))
		default:
			lines = append(lines, fmt.Sprintf("%s %s, reply to %s: %s (post %s)", who, when, post.RootId, body, post.Id))
			prevThread = post.RootId
		}
		var files []*model.FileInfo
		if post.Metadata != nil {
			files = post.Metadata.Files
		}
		for _, info := range files {
			lines = append(lines, fmt.Sprintf("  [file] %s (%s, %s, id: %s)",
				info.Name, info.MimeType, HumanSize(info.Size), info.Id))
		}
		// The server can list an attachment in file_ids yet send no metadata for it.
		if missing := len(post.FileIds) - len(files); missing > 0 {
			lines = append(lines, fmt.Sprintf("  [file] (%d unavailable — server sent no metadata)", missing))
		}
		if reactions := reactionSummary(post); reactions != "" {
			lines = append(lines, "  [reactions] "+reactions)
		}
	}
	if !opts.NoPaging && (total > len(shown) || list.PrevPostId != "") {
		lines = append(lines, fmt.Sprintf("(%s shown — older posts exist, pass before=%s)",
			Plural(len(shown), "post", "posts"), shown[0].Id))
	}
	return strings.Join(lines, "\n")
}

// reactionSummary renders `:thumbsup: 2 :eyes: 1` in first-seen order, or ""
// when nobody reacted.
func reactionSummary(post *model.Post) string {
	if post.Metadata == nil {
		return ""
	}
	var order []string
	counts := map[string]int{}
	for _, r := range post.Metadata.Reactions {
		if counts[r.EmojiName] == 0 {
			order = append(order, r.EmojiName)
		}
		counts[r.EmojiName]++
	}
	parts := make([]string, 0, len(order))
	for _, emoji := range order {
		parts = append(parts, fmt.Sprintf(":%s: %d", emoji, counts[emoji]))
	}
	return strings.Join(parts, " ")
}

// FormatPosts looks up the authors of the posts that will be shown (see
// AuthorNames) and renders list with the package-level FormatPosts at the current time.
func (c *Context) FormatPosts(ctx context.Context, list *model.PostList, opts PostsOptions) (string, error) {
	shown, _ := ShownPosts(list, opts.Limit)
	ids := make([]string, 0, len(shown))
	for _, post := range shown {
		ids = append(ids, post.UserId)
	}
	names, err := c.AuthorNames(ctx, ids)
	if err != nil {
		return "", err
	}
	return FormatPosts(list, names, c.now(), opts), nil
}

// ChannelPosts fetches the newest perPage posts of a channel, or those older
// than post before when it is set (GET /channels/{id}/posts), without the rest
// of their threads (see getPostList).
func (c *Context) ChannelPosts(ctx context.Context, channelID, before string, perPage int) (*model.PostList, error) {
	q := url.Values{}
	q.Set("page", "0")
	q.Set("per_page", strconv.Itoa(perPage))
	if before != "" {
		q.Set("before", before)
	}
	q.Set("include_deleted", "false")
	return c.getPostList(ctx, "/channels/"+channelID+"/posts", q)
}

// getPostList GETs a channel post list at path (relative to /api/v4) with q.
// Client4's methods cannot send skipFetchThreads=true, without which the
// server adds every post of each listed post's thread to the response; only
// the listed posts (list.Order) are rendered, and they carry root_id and
// reply_count either way.
func (c *Context) getPostList(ctx context.Context, path string, q url.Values) (*model.PostList, error) {
	q.Set("collapsedThreads", "false")
	q.Set("skipFetchThreads", "true")
	resp, err := c.client.DoAPIGet(ctx, path+"?"+q.Encode(), "")
	if err != nil {
		return nil, WrapErr("/api/v4"+path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	list, _, err := model.DecodeJSONFromResponse[*model.PostList](resp)
	if err != nil {
		return nil, WrapErr("/api/v4"+path, err)
	}
	return list, nil
}
