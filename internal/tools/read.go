package tools

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/Chi-teck/mm-mcp/internal/mattermost"
)

const (
	// defaultLimit is the schema default of the post `limit` argument.
	defaultLimit = 30
	// maxLimit is the upper bound of every post `limit` argument.
	maxLimit = 200
	// noChannels is the list_channels result for a user without channels in the team.
	noChannels = "No channels found."
	// maxHits caps the search results.
	maxHits = 30
	// searchHint is the truncation hint of a search hit body.
	searchHint = "read it with read_posts full=true"
	// memberPage and maxMembers bound list_members without query. maxMembers is a
	// multiple of memberPage, so the pages end exactly at the cap.
	memberPage = 200
	maxMembers = 1000
	// notInChannel marks autocomplete matches outside the channel.
	notInChannel = " [NOT in channel — mentions won't notify]"
)

type listChannelsIn struct{}

type readPostsIn struct {
	Channel      string `json:"channel" jsonschema:"Channel name or 26-char id"`
	ThreadRootID string `json:"thread_root_id,omitempty" jsonschema:"Read this thread: its root post plus the newest replies (up to limit)"`
	Pinned       bool   `json:"pinned,omitempty" jsonschema:"Read only pinned posts"`
	Before       string `json:"before,omitempty" jsonschema:"Read the posts older than this post id (page backwards)"`
	Since        string `json:"since,omitempty" jsonschema:"Posts since this time: 2h, 30m, 45s, 3d, ISO date/datetime, or epoch ms"`
	Limit        int    `json:"limit,omitempty" jsonschema:"Max posts to fetch and show (default 30, max 200); on a thread it counts replies and the root is shown on top of them"`
	Full         bool   `json:"full,omitempty" jsonschema:"Print message bodies in full instead of cutting them at 500 chars"`
}

type getPostIn struct {
	PostID string `json:"post_id" jsonschema:"26-char post id"`
	Full   bool   `json:"full,omitempty" jsonschema:"Print the message body in full instead of cutting it at 500 chars"`
}

// registerRead adds list_channels, read_posts, get_post, search and list_members.
func registerRead(s *mcp.Server, c *mattermost.Context) {
	addTool(s, "list_channels",
		"List the Mattermost channels this user is a member of in the team, as `name — display_name [type]` "+
			"(a DM shows `DM with @user` as its display name).",
		func(ctx context.Context, _ listChannelsIn) (string, error) { return listChannels(ctx, c) })
	addTool(s, "read_posts",
		"Read posts from a Mattermost channel. Branches: thread_root_id → a thread's root plus its newest replies; "+
			"pinned → pinned posts; before → posts older than a post id; since → posts since a time "+
			"(\"2h\", \"30m\", ISO date, epoch ms); otherwise latest posts (limit, default 30). "+
			"Bodies are cut at 500 chars unless full=true.",
		func(ctx context.Context, in readPostsIn) (string, error) { return readPosts(ctx, c, in) },
		limitRange, defaultTo("limit", defaultLimit))
	addTool(s, "get_post",
		"Read one Mattermost post by id — its channel, author, body, reactions and attachments. Use it to check "+
			"a single post instead of reading the channel and filtering.",
		func(ctx context.Context, in getPostIn) (string, error) { return getPost(ctx, c, in) })
	addTool(s, "search",
		`Search Mattermost posts or files across the team by keyword (type: "posts" or "files").`,
		func(ctx context.Context, in searchIn) (string, error) { return search(ctx, c, in) },
		enum("type", "posts", "files"))
	addTool(s, "list_members",
		"List members of a Mattermost channel; with query, fuzzy-match usernames (out-of-channel matches are marked).",
		func(ctx context.Context, in listMembersIn) (string, error) { return listMembers(ctx, c, in) })
}

func listChannels(ctx context.Context, c *mattermost.Context) (string, error) {
	team := c.Team()
	channels, _, err := c.Client().GetChannelsForTeamForUser(ctx, team.Id, "me", false, "")
	if err != nil {
		return "", mattermost.WrapErr("/api/v4/users/me/teams/"+team.Id+"/channels", err)
	}
	if len(channels) == 0 {
		return noChannels, nil
	}
	labels, err := c.DMLabels(ctx, channels)
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(channels))
	for _, ch := range channels {
		display := ch.DisplayName
		if label, ok := labels[ch.Id]; ok {
			display = label
		} else if display == "" {
			display = ch.Name
		}
		lines = append(lines, fmt.Sprintf("- %s — %s [%s]", ch.Name, display, ch.Type))
	}
	return strings.Join(lines, "\n"), nil
}

func readPosts(ctx context.Context, c *mattermost.Context, in readPostsIn) (string, error) {
	if in.ThreadRootID != "" {
		if err := checkID("thread root id", in.ThreadRootID); err != nil {
			return "", err
		}
	}
	ch, err := c.ResolveChannel(ctx, in.Channel)
	if err != nil {
		return "", err
	}
	limit := in.Limit
	client := c.Client()
	opts := mattermost.PostsOptions{Limit: limit, Full: in.Full}
	var list *model.PostList
	note := ""
	switch {
	case in.ThreadRootID != "":
		// direction=up pages from the newest reply; threadNote covers how the server counts the root.
		list, _, err = client.GetPostThreadWithOpts(ctx, in.ThreadRootID, "",
			model.GetPostsOptions{PerPage: limit, Direction: "up"})
		if err != nil {
			return "", mattermost.WrapErr("/api/v4/posts/"+in.ThreadRootID+"/thread", err)
		}
		root := list.Posts[in.ThreadRootID]
		if root == nil || root.ChannelId != ch.Id {
			return "", fmt.Errorf("thread root %s is not in %s", in.ThreadRootID, c.ChannelLabel(ctx, ch))
		}
		// The thread of a reply is its root's; the reply itself is in the response.
		if root.RootId != "" {
			return "", fmt.Errorf("post %s is a reply, not a thread root — its thread root id is %s", in.ThreadRootID, root.RootId)
		}
		note = threadNote(list, in.ThreadRootID, limit)
		opts = mattermost.PostsOptions{Limit: limit + 1, Full: in.Full, NoPaging: true}
	case in.Pinned:
		list, _, err = client.GetPinnedPosts(ctx, ch.Id, "")
		if err != nil {
			return "", mattermost.WrapErr("/api/v4/channels/"+ch.Id+"/pinned", err)
		}
		// `pinned` outranks `before`, so a before= hint would loop: say how many are hidden instead.
		note = pinnedNote(list, limit)
		opts.NoPaging = true
	case in.Before != "":
		list, err = c.ChannelPosts(ctx, ch.Id, in.Before, limit)
		if err != nil {
			return "", err
		}
	case in.Since != "":
		since, perr := mattermost.ParseSince(in.Since, time.Now())
		if perr != nil {
			return "", perr
		}
		// Not ?since=: it selects by update_at, so old posts with new reactions or replies come back.
		// One post beyond limit tells whether the next page still reaches into the window.
		list, err = c.ChannelPosts(ctx, ch.Id, "", min(limit+1, maxLimit))
		if err != nil {
			return "", err
		}
		cutSince(list, since)
	default:
		list, err = c.ChannelPosts(ctx, ch.Id, "", limit)
		if err != nil {
			return "", err
		}
	}
	out, err := c.FormatPosts(ctx, list, opts)
	if err != nil {
		return "", err
	}
	return out + note, nil
}

// cutSince drops the posts created before since (epoch ms) from list. A page that reaches back
// past since has nothing older to offer, so its paging hint goes too.
func cutSince(list *model.PostList, since int64) {
	order := list.Order[:0]
	for _, id := range list.Order {
		if post := list.Posts[id]; post != nil && post.CreateAt < since {
			list.PrevPostId = ""
			continue
		}
		order = append(order, id)
	}
	list.Order = order
}

// threadNote is the line appended when a thread has more replies than limit.
// The server counts the root inside its perPage+1 window, so has_next is also set when exactly
// limit replies exist; the root's reply_count decides when known.
func threadNote(list *model.PostList, rootID string, limit int) string {
	if list.HasNext == nil || !*list.HasNext {
		return ""
	}
	of := ""
	if root := list.Posts[rootID]; root != nil && root.ReplyCount > 0 {
		if root.ReplyCount <= int64(limit) {
			return ""
		}
		of = fmt.Sprintf(" of %d", root.ReplyCount)
	}
	more := "pass limit=200 for more"
	if limit >= maxLimit {
		more = "older replies are out of reach"
	}
	return fmt.Sprintf("\n(newest %d replies shown%s — %s)", limit, of, more)
}

// pinnedNote is the line appended when there are more pinned posts than limit.
func pinnedNote(list *model.PostList, limit int) string {
	_, total := mattermost.ShownPosts(list, limit)
	if total <= limit {
		return ""
	}
	more := "pass limit=200 for more"
	if limit >= maxLimit {
		more = "older pinned posts are out of reach"
	}
	return fmt.Sprintf("\n(newest %d of %d pinned posts shown — %s)", limit, total, more)
}

func getPost(ctx context.Context, c *mattermost.Context, in getPostIn) (string, error) {
	if err := checkID("post id", in.PostID); err != nil {
		return "", err
	}
	post, _, err := c.Client().GetPost(ctx, in.PostID, "")
	if err != nil {
		err = mattermost.WrapErr("/api/v4/posts/"+in.PostID, err)
		// A deleted post and one in an unreadable channel both answer 404 with a message
		// that tells neither apart.
		if mattermost.IsNotFound(err) {
			return "", fmt.Errorf("post %s not found — it is deleted, or in a channel this user cannot read", in.PostID)
		}
		return "", err
	}
	// The channel name is decoration: when it cannot be resolved, the id stands in.
	name := post.ChannelId
	if ch, cerr := c.ResolveChannel(ctx, post.ChannelId); cerr == nil {
		name = c.ChannelLabel(ctx, ch)
	}
	list := model.NewPostList()
	list.AddPost(post)
	list.AddOrder(post.Id)
	body, err := c.FormatPosts(ctx, list, mattermost.PostsOptions{Limit: 1, Full: in.Full, NoPaging: true})
	if err != nil {
		return "", err
	}
	return "in " + name + ":\n" + body, nil
}

type searchIn struct {
	Query string `json:"query" jsonschema:"Search terms"`
	Type  string `json:"type" jsonschema:"Search posts or files"`
}

type listMembersIn struct {
	Channel string `json:"channel" jsonschema:"Channel name or 26-char id"`
	Query   string `json:"query,omitempty" jsonschema:"Fuzzy username/name filter"`
}

// enum restricts the string property prop to values.
func enum(prop string, values ...string) schemaEdit {
	return func(s *jsonschema.Schema) {
		p := s.Properties[prop]
		for _, v := range values {
			p.Enum = append(p.Enum, v)
		}
	}
}

func search(ctx context.Context, c *mattermost.Context, in searchIn) (string, error) {
	team := c.Team()
	if in.Type == "files" {
		return searchFiles(ctx, c, team.Id, in.Query)
	}
	list, _, err := c.Client().SearchPosts(ctx, team.Id, in.Query, false)
	if err != nil {
		return "", mattermost.WrapErr("/api/v4/teams/"+team.Id+"/posts/search", err)
	}
	// Newest first, unlike ShownPosts: hits span channels and are ranked by recency.
	posts := mattermost.LivePosts(list)
	sort.SliceStable(posts, func(i, j int) bool { return posts[i].CreateAt > posts[j].CreateAt })
	if len(posts) > maxHits {
		posts = posts[:maxHits]
	}
	if len(posts) == 0 {
		return `No posts found for "` + in.Query + `".`, nil
	}
	userIDs := make([]string, 0, len(posts))
	channelIDs := make([]string, 0, len(posts))
	for _, p := range posts {
		userIDs = append(userIDs, p.UserId)
		channelIDs = append(channelIDs, p.ChannelId)
	}
	users, err := c.AuthorNames(ctx, userIDs)
	if err != nil {
		return "", err
	}
	channels := c.ChannelNames(ctx, channelIDs)
	now := time.Now()
	lines := make([]string, 0, len(posts))
	for _, p := range posts {
		who := mattermost.AuthorName(users, p.UserId)
		body := mattermost.Truncate(p.Message, searchHint, mattermost.MaxBodyChars)
		lines = append(lines, fmt.Sprintf("**%s** (%s) in %s: %s (post %s)",
			who, mattermost.RelTime(p.CreateAt, now), channels[p.ChannelId], body, p.Id))
	}
	return strings.Join(lines, "\n"), nil
}

func searchFiles(ctx context.Context, c *mattermost.Context, teamID, query string) (string, error) {
	list, _, err := c.Client().SearchFiles(ctx, teamID, query, false)
	if err != nil {
		return "", mattermost.WrapErr("/api/v4/teams/"+teamID+"/files/search", err)
	}
	var infos []*model.FileInfo
	for _, id := range list.Order {
		if info := list.FileInfos[id]; info != nil && len(infos) < maxHits {
			infos = append(infos, info)
		}
	}
	if len(infos) == 0 {
		return `No files found for "` + query + `".`, nil
	}
	channelIDs := make([]string, 0, len(infos))
	for _, info := range infos {
		channelIDs = append(channelIDs, info.ChannelId)
	}
	channels := c.ChannelNames(ctx, channelIDs)
	lines := make([]string, 0, len(infos))
	for _, info := range infos {
		lines = append(lines, fmt.Sprintf("[file] %s (%s, %s, id: %s) [%s]",
			info.Name, info.MimeType, mattermost.HumanSize(info.Size), info.Id, channels[info.ChannelId]))
	}
	return strings.Join(lines, "\n"), nil
}

func listMembers(ctx context.Context, c *mattermost.Context, in listMembersIn) (string, error) {
	ch, err := c.ResolveChannel(ctx, in.Channel)
	if err != nil {
		return "", err
	}
	if in.Query != "" {
		return matchMembers(ctx, c, ch, in.Query)
	}
	var lines []string
	for page := 0; ; page++ {
		users, _, err := c.Client().GetUsersInChannel(ctx, ch.Id, page, memberPage, "")
		if err != nil {
			return "", mattermost.WrapErr("/api/v4/users", err)
		}
		for _, u := range users {
			lines = append(lines, memberLine(u))
		}
		if len(users) < memberPage {
			break
		}
		if len(lines) >= maxMembers {
			// A full last page is no proof of truncation: ask for the one member after the cap
			// (page=maxMembers, per_page=1); past the end the server returns [].
			more, _, err := c.Client().GetUsersInChannel(ctx, ch.Id, maxMembers, 1, "")
			if err != nil {
				return "", mattermost.WrapErr("/api/v4/users", err)
			}
			if len(more) > 0 {
				lines = append(lines, fmt.Sprintf("(first %d members — pass query to search)", maxMembers))
			}
			break
		}
	}
	if len(lines) == 0 {
		return "No members found.", nil
	}
	return strings.Join(lines, "\n"), nil
}

// matchMembers is list_members with query: the server's user autocomplete in team + channel,
// with its default limit (the limit parameter is left out on purpose).
func matchMembers(ctx context.Context, c *mattermost.Context, ch *model.Channel, query string) (string, error) {
	team := c.Team()
	q := url.Values{}
	q.Set("in_team", team.Id)
	q.Set("in_channel", ch.Id)
	q.Set("name", query)
	resp, err := c.Client().DoAPIGet(ctx, "/users/autocomplete?"+q.Encode(), "")
	if err != nil {
		return "", mattermost.WrapErr("/api/v4/users/autocomplete", err)
	}
	defer func() { _ = resp.Body.Close() }()
	match, _, err := model.DecodeJSONFromResponse[*model.UserAutocomplete](resp)
	if err != nil {
		return "", mattermost.WrapErr("/api/v4/users/autocomplete", err)
	}
	var lines []string
	if match != nil {
		for _, u := range match.Users {
			lines = append(lines, memberLine(u))
		}
		for _, u := range match.OutOfChannel {
			lines = append(lines, memberLine(u)+notInChannel)
		}
	}
	if len(lines) == 0 {
		return `No users matching "` + query + `".`, nil
	}
	return strings.Join(lines, "\n"), nil
}

// memberLine renders `- <username> — <first last>`, or `- <username>` without a name.
func memberLine(u *model.User) string {
	full := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if full == "" {
		return "- " + u.Username
	}
	return "- " + u.Username + " — " + full
}
