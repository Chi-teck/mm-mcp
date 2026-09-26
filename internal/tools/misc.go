package tools

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/mattermost"
)

type threadIn struct {
	ThreadRootID string `json:"thread_root_id" jsonschema:"Root post id of the thread"`
}

type reactIn struct {
	PostID string `json:"post_id" jsonschema:"26-char post id"`
	Emoji  string `json:"emoji" jsonschema:"Emoji name without colons, e.g. thumbsup"`
	Action string `json:"action" jsonschema:"add or remove the reaction"`
}

type editPostIn struct {
	PostID  string `json:"post_id" jsonschema:"26-char post id"`
	Action  string `json:"action" jsonschema:"edit (needs message) or delete"`
	Message string `json:"message,omitempty" jsonschema:"New message text (required for edit)"`
}

type dmIn struct {
	Username string `json:"username" jsonschema:"Username, e.g. ivan.ch (a leading @ is ignored)"`
}

// registerMisc adds follow_thread, unfollow_thread, react, edit_post and dm.
func registerMisc(s *mcp.Server, c *mattermost.Context) {
	addTool(s, "follow_thread", "Follow a Mattermost thread, so you stay subscribed to its replies.",
		func(ctx context.Context, in threadIn) (string, error) {
			return followThread(ctx, c, in.ThreadRootID, true)
		})
	addTool(s, "unfollow_thread",
		"Stop following a Mattermost thread — the way out of a conversation you were pulled into.",
		func(ctx context.Context, in threadIn) (string, error) {
			return followThread(ctx, c, in.ThreadRootID, false)
		})
	addTool(s, "react",
		"Add or remove an emoji reaction on a Mattermost post (emoji name without colons, e.g. thumbsup).",
		func(ctx context.Context, in reactIn) (string, error) { return react(ctx, c, in) },
		enum("action", "add", "remove"))
	addTool(s, "edit_post", "Edit or delete one of your own Mattermost posts.",
		func(ctx context.Context, in editPostIn) (string, error) { return editPost(ctx, c, in) },
		enum("action", "edit", "delete"))
	addTool(s, "dm",
		"Open (or reuse) a direct-message channel with a user by username (a leading @ is ignored) and return its channel name.",
		func(ctx context.Context, in dmIn) (string, error) { return openDM(ctx, c, in.Username) })
}

// checkID refuses value unless it is a 26-char id. Ids become path segments of authenticated
// requests; Client4 escapes a segment and strips "..", but a leftover "." segment is still
// resolved away and shifts the request to another endpoint.
func checkID(what, value string) error {
	if mattermost.IsID(value) {
		return nil
	}
	return fmt.Errorf("invalid %s: %q (expected a 26-char id)", what, value)
}

// followThread follows (follow=true) or leaves the thread rooted at rootID. The server answers 200
// for a reply or a root without replies but has no thread to change, so the root is checked first.
func followThread(ctx context.Context, c *mattermost.Context, rootID string, follow bool) (string, error) {
	if err := checkID("thread root id", rootID); err != nil {
		return "", err
	}
	root, err := threadRoot(ctx, c, rootID)
	if err != nil {
		return "", err
	}
	if root.ReplyCount == 0 {
		if follow {
			return "", fmt.Errorf("post %s has no replies yet, so there is no thread to follow. "+
				"Mattermost follows it automatically for its author and for anyone who replies", rootID)
		}
		return "Post " + rootID + " has no replies yet, so there is no thread to leave — nothing to do.", nil
	}
	me := c.Me()
	team := c.Team()
	path := fmt.Sprintf("/api/v4/users/%s/teams/%s/threads/%s/following", me.Id, team.Id, rootID)
	if _, err := c.Client().UpdateThreadFollowForUser(ctx, me.Id, team.Id, rootID, follow); err != nil {
		return "", mattermost.WrapErr(path, err)
	}
	if follow {
		return "Following thread " + rootID, nil
	}
	return "Left thread " + rootID + " — posting in it again re-follows it.", nil
}

// react adds or removes the user's emoji reaction; a remove first checks the reaction exists,
// since the server answers 200 either way. The emoji is normalized (colons stripped, lowercased)
// so ":ThumbsUp:" matches the stored "thumbsup".
func react(ctx context.Context, c *mattermost.Context, in reactIn) (string, error) {
	if err := checkID("post id", in.PostID); err != nil {
		return "", err
	}
	emoji := strings.ToLower(strings.Trim(in.Emoji, ":"))
	me := c.Me()
	reaction := &model.Reaction{UserId: me.Id, PostId: in.PostID, EmojiName: emoji}
	if in.Action == "add" {
		if _, _, err := c.Client().SaveReaction(ctx, reaction); err != nil {
			err = mattermost.WrapErr("/api/v4/reactions", err)
			// A 404 doesn't tell a missing post from an unknown emoji; only a custom emoji can be unknown.
			if mattermost.IsNotFound(err) && !model.IsSystemEmojiName(emoji) {
				_, _, lookupErr := c.Client().GetEmojiByName(ctx, emoji)
				if mattermost.IsNotFound(mattermost.WrapErr("/api/v4/emoji/name/"+url.PathEscape(emoji), lookupErr)) {
					return "", fmt.Errorf("unknown emoji %q", emoji)
				}
			}
			return "", err
		}
		return fmt.Sprintf("Added :%s: on post %s", emoji, in.PostID), nil
	}
	existing, _, err := c.Client().GetReactions(ctx, in.PostID)
	if err != nil {
		return "", mattermost.WrapErr("/api/v4/posts/"+in.PostID+"/reactions", err)
	}
	found := false
	for _, r := range existing {
		if r.UserId == me.Id && r.EmojiName == emoji {
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("No :%s: reaction by you on post %s — nothing to remove.", emoji, in.PostID), nil
	}
	if _, err := c.Client().DeleteReaction(ctx, reaction); err != nil {
		path := fmt.Sprintf("/api/v4/users/%s/posts/%s/reactions/%s", me.Id, in.PostID, url.PathEscape(emoji))
		return "", mattermost.WrapErr(path, err)
	}
	return fmt.Sprintf("Removed :%s: from post %s", emoji, in.PostID), nil
}

// editPost patches the message of a post or deletes it; deleting a missing post is a no-op.
// Only the user's own posts are changed: a token allowed to edit other people's posts
// would otherwise let a hostile message steer the model into changing them.
func editPost(ctx context.Context, c *mattermost.Context, in editPostIn) (string, error) {
	if err := checkID("post id", in.PostID); err != nil {
		return "", err
	}
	if in.Action == "edit" && in.Message == "" {
		return "", errors.New("edit requires a message")
	}
	gone := "Post " + in.PostID + " is already deleted or does not exist — nothing to do."
	post, _, err := c.Client().GetPost(ctx, in.PostID, "")
	if err != nil {
		err = mattermost.WrapErr("/api/v4/posts/"+in.PostID, err)
		if !mattermost.IsNotFound(err) {
			return "", err
		}
		if in.Action == "edit" {
			return "", fmt.Errorf("post %s not found — it is deleted, or in a channel this user cannot read", in.PostID)
		}
		return gone, nil
	}
	if me := c.Me(); post.UserId != me.Id {
		names, err := c.AuthorNames(ctx, []string{post.UserId})
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("post %s was written by @%s, not by you — edit_post only changes your own posts",
			in.PostID, mattermost.AuthorName(names, post.UserId))
	}
	if in.Action == "edit" {
		patch := &model.PostPatch{Message: &in.Message}
		if _, _, err := c.Client().PatchPost(ctx, in.PostID, patch); err != nil {
			return "", mattermost.WrapErr("/api/v4/posts/"+in.PostID+"/patch", err)
		}
		return "Edited post " + in.PostID, nil
	}
	if _, err := c.Client().DeletePost(ctx, in.PostID); err != nil {
		err = mattermost.WrapErr("/api/v4/posts/"+in.PostID, err)
		if mattermost.IsNotFound(err) {
			return gone, nil
		}
		return "", err
	}
	return "Deleted post " + in.PostID, nil
}

// openDM opens or reuses the direct channel between the user and username; one leading "@" is
// stripped.
func openDM(ctx context.Context, c *mattermost.Context, username string) (string, error) {
	username = strings.TrimPrefix(username, "@")
	if err := mattermost.CheckName("username", username); err != nil {
		return "", err
	}
	user, _, err := c.Client().GetUserByUsername(ctx, username, "")
	if err != nil {
		err = mattermost.WrapErr("/api/v4/users/username/"+url.PathEscape(username), err)
		if mattermost.IsNotFound(err) {
			return "", fmt.Errorf("no user @%s", username)
		}
		return "", err
	}
	if !strings.EqualFold(user.Username, username) {
		return "", fmt.Errorf("mattermost returned @%s for username %q; refusing to open the DM", user.Username, username)
	}
	me := c.Me()
	ch, _, err := c.Client().CreateDirectChannel(ctx, me.Id, user.Id)
	if err != nil {
		return "", mattermost.WrapErr("/api/v4/channels/direct", err)
	}
	return fmt.Sprintf("DM channel with @%s: %s (id: %s) — pass it as the channel to create_post",
		user.Username, ch.Name, ch.Id), nil
}
