package mattermost

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Chi-teck/mm-mcp/internal/config"
)

// CacheTTL is the lifetime of cached channels and usernames.
const CacheTTL = 60 * time.Second

// maxSuggestions caps the "Did you mean" list of ResolveChannel.
const maxSuggestions = 5

// Context is the per-process state shared by all tools: configuration, the
// Mattermost client and the caches. It is safe for concurrent use.
type Context struct {
	cfg    config.Config
	client *model.Client4
	now    func() time.Time // injectable for cache TTL tests

	me            *model.User // set once by Init
	team          *model.Team // set once by Init
	serverVersion string      // set once by Init; "" when unknown

	mu       sync.Mutex
	channels map[string]cached[*model.Channel] // keys "id:<id>" and "name:<team id>:<name>" ("" team for DM/GM)
	users    map[string]cached[string]         // user id → username
	maxFile  cached[int64]                     // server MaxFileSize; zero value = not cached
}

type cached[T any] struct {
	val     T
	expires time.Time
}

// NewContext builds a Context from cfg. It does no network I/O; call Init
// at startup to fetch the current user and team.
func NewContext(cfg config.Config) *Context {
	return &Context{
		cfg:      cfg,
		client:   newClient4(cfg.URL, cfg.Token),
		now:      time.Now,
		channels: map[string]cached[*model.Channel]{},
		users:    map[string]cached[string]{},
	}
}

// Client returns the shared Client4. Wrap errors of direct calls with WrapErr.
func (c *Context) Client() *model.Client4 { return c.client }

// URL returns the server base URL without a trailing slash.
func (c *Context) URL() string { return c.cfg.URL }

// TeamName returns the configured team name.
func (c *Context) TeamName() string { return c.cfg.Team }

// DownloadDir returns the absolute download directory, or "" when unset.
func (c *Context) DownloadDir() string { return c.cfg.DownloadDir }

// UploadRoot returns the absolute, symlink-resolved upload root, or "" when attachments are disabled.
func (c *Context) UploadRoot() string { return c.cfg.UploadRoot }

// Init fetches the current user and the configured team, so that a bad token
// or team name fails at startup. It must succeed before any tool runs; the
// results are kept for the process lifetime. The caller bounds ctx.
// A team containing ".." is refused before any request (see CheckName).
func (c *Context) Init(ctx context.Context) error {
	if err := CheckName("team", c.cfg.Team); err != nil {
		return err
	}
	me, resp, err := c.client.GetMe(ctx, "")
	if err != nil {
		return WrapErr("/api/v4/users/me", err)
	}
	if me == nil {
		return errEmptyResponse
	}
	team, err := c.fetchTeam(ctx)
	if err != nil {
		return err
	}
	if team == nil {
		return errEmptyResponse
	}
	c.me, c.team = me, team
	if resp != nil {
		c.serverVersion = trimVersion(resp.ServerVersion)
	}
	return nil
}

// trimVersion keeps the `X.Y.Z` version from an X-Version-Id header
// (`<X.Y.Z>.<build>.<hash>.<licensed>`), or returns "" when the header does
// not start with three numbers.
func trimVersion(id string) string {
	parts := strings.SplitN(id, ".", 4)
	if len(parts) < 3 {
		return ""
	}
	for _, p := range parts[:3] {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return ""
		}
	}
	return strings.Join(parts[:3], ".")
}

// Me returns the current user loaded by Init.
func (c *Context) Me() *model.User { return c.me }

// Team returns the configured team loaded by Init.
func (c *Context) Team() *model.Team { return c.team }

// ServerVersion returns the Mattermost version (`X.Y.Z`) seen by Init, or ""
// when the server did not report a recognizable one.
func (c *Context) ServerVersion() string { return c.serverVersion }

// fetchTeam looks up the configured team by id when the configured value has
// id shape and by name otherwise. An HTTP 404 yields
// `Mattermost team not found: <team>`.
func (c *Context) fetchTeam(ctx context.Context) (*model.Team, error) {
	ref := c.cfg.Team
	var (
		t    *model.Team
		err  error
		path string
	)
	if IsID(ref) {
		t, _, err = c.client.GetTeam(ctx, ref, "")
		path = "/api/v4/teams/" + ref
	} else {
		t, _, err = c.client.GetTeamByName(ctx, ref, "")
		path = "/api/v4/teams/name/" + url.PathEscape(ref)
	}
	if err == nil {
		return t, nil
	}
	err = WrapErr(path, err)
	if IsNotFound(err) {
		return nil, fmt.Errorf("mattermost team not found: %s", ref)
	}
	return nil, err
}

// IsID reports whether s has the shape of a Mattermost id (^[a-z0-9]{26}$).
func IsID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// CheckName refuses a name bound for a Client4 path segment if it contains
// "..": Client4 strips every ".." from segments, which would silently address
// another object (mm..test -> mmtest).
func CheckName(what, name string) error {
	if strings.Contains(name, "..") {
		return fmt.Errorf("invalid %s: %q (\"..\" is not allowed)", what, name)
	}
	return nil
}

// ResolveChannel looks up a channel by 26-char id or by name in the
// configured team (archived channels included either way), falling back to an exact name match among the user's own
// channels (so DM channel names always resolve). An id-shaped value that is
// not found as an id is looked up as a name, since such names are legal.
// Results are cached for CacheTTL. An unknown name yields
// `channel not found: <name>`, followed by ` (did you mean: a, b)` when the
// user's own channels in the team contain the name as a case-insensitive
// substring of their name or display name (at most maxSuggestions, ranked
// by suggestionRank, then alphabetically). An empty channel is refused.
func (c *Context) ResolveChannel(ctx context.Context, channel string) (*model.Channel, error) {
	if strings.TrimSpace(channel) == "" {
		return nil, errors.New("channel is required (a name or id)")
	}
	if IsID(channel) {
		if ch, ok := c.cachedChannel("id:" + channel); ok {
			return ch, nil
		}
		ch, _, err := c.client.GetChannel(ctx, channel)
		if err == nil {
			c.storeChannels(ch)
			return ch, nil
		}
		err = WrapErr("/api/v4/channels/"+channel, err)
		if !IsNotFound(err) {
			return nil, err
		}
		// Not an id: fall through to the lookup by name.
	}

	if err := CheckName("channel name", channel); err != nil {
		return nil, err
	}
	team := c.Team()
	// Names are unique only within a team: never return another team's channel.
	for _, teamID := range []string{team.Id, ""} {
		if ch, ok := c.cachedChannel("name:" + teamID + ":" + channel); ok {
			return ch, nil
		}
	}
	// Include archived channels: the lookup by id returns them too.
	ch, _, err := c.client.GetChannelByNameIncludeDeleted(ctx, channel, team.Id, "")
	if err == nil {
		c.storeChannels(ch)
		return ch, nil
	}
	err = WrapErr("/api/v4/teams/"+team.Id+"/channels/name/"+url.PathEscape(channel), err)
	if !IsNotFound(err) {
		return nil, err
	}
	return c.resolveFromMyChannels(ctx, channel, team.Id)
}

// ChannelNames maps channel ids to names for decoration: an id that cannot be
// resolved maps to itself, and a DM channel's name carries its label (see
// ChannelLabel). Ids not in the cache are looked up in one request
// for the user's channels in the configured team (DM and GM channels and
// archived ones included), which covers team-scoped search hits; only ids
// still missing after it are resolved one by one.
func (c *Context) ChannelNames(ctx context.Context, ids []string) map[string]string {
	names := make(map[string]string, len(ids))
	for _, id := range ids {
		if _, ok := c.cachedChannel("id:" + id); !ok {
			team, me := c.Team(), c.Me()
			// On failure the per-id lookups below still run.
			if chans, _, err := c.client.GetChannelsForTeamForUser(ctx, team.Id, me.Id, true, ""); err == nil {
				c.storeChannels(chans...)
			}
			break
		}
	}
	var chans []*model.Channel
	for _, id := range ids {
		if _, ok := names[id]; ok {
			continue
		}
		names[id] = id
		if ch, err := c.ResolveChannel(ctx, id); err == nil {
			names[id] = ch.Name
			chans = append(chans, ch)
		}
	}
	// Only a canceled ctx fails the lookup; the names alone are then left.
	labels, _ := c.DMLabels(ctx, chans)
	for id, label := range labels {
		names[id] += " (" + label + ")"
	}
	return names
}

// ChannelLabel is the name of ch for decoration; a DM channel's name is
// followed by its label, as in `<name> (DM with @bob)`.
func (c *Context) ChannelLabel(ctx context.Context, ch *model.Channel) string {
	labels, _ := c.DMLabels(ctx, []*model.Channel{ch})
	if label, ok := labels[ch.Id]; ok {
		return ch.Name + " (" + label + ")"
	}
	return ch.Name
}

// DMLabels maps the id of each direct channel in chans to its label:
// `DM with @<username>`, or `DM with yourself` for the self-DM. Other channels,
// and DM channels whose name holds no member ids, are absent. The other
// members are looked up as in AuthorNames: an unresolved one is shown by id.
func (c *Context) DMLabels(ctx context.Context, chans []*model.Channel) (map[string]string, error) {
	me := c.Me().Id
	others := make(map[string]string) // channel id → the other member's user id
	var ids []string
	for _, ch := range chans {
		// The self-DM yields (me, ""); GetOtherUserIdForDM would drop it.
		first, second := ch.GetBothUsersForDM()
		if first == "" {
			continue
		}
		other := first
		if first == me && second != "" {
			other = second
		}
		others[ch.Id] = other
		if other != me {
			ids = append(ids, other)
		}
	}
	if len(others) == 0 {
		return nil, nil
	}
	names, err := c.AuthorNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	labels := make(map[string]string, len(others))
	for chID, other := range others {
		if other == me {
			labels[chID] = "DM with yourself"
		} else {
			labels[chID] = "DM with @" + AuthorName(names, other)
		}
	}
	return labels, nil
}

// IsNotFound reports whether err is an *APIError with status 404.
func IsNotFound(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	return ok && apiErr.Status == http.StatusNotFound
}

// resolveFromMyChannels is the fallback after a 404 by name: it returns the
// user's own channel with that exact name (DM channels, which have no team,
// may be missing from the by-name route), or the not-found error with
// suggestions.
func (c *Context) resolveFromMyChannels(ctx context.Context, name, teamID string) (*model.Channel, error) {
	me := c.Me()
	chans, _, err := c.client.GetChannelsForTeamForUser(ctx, teamID, me.Id, false, "")
	if err != nil {
		return nil, WrapErr("/api/v4/users/"+me.Id+"/teams/"+teamID+"/channels", err)
	}
	c.storeChannels(chans...)

	type match struct {
		rank int
		name string
	}
	var ranked []match
	for _, ch := range chans {
		if ch.Name == name {
			return ch, nil
		}
		if rank, ok := suggestionRank(name, ch); ok {
			ranked = append(ranked, match{rank, ch.Name})
		}
	}
	slices.SortFunc(ranked, func(a, b match) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), strings.Compare(a.name, b.name))
	})
	var matches []string
	for _, m := range ranked {
		if len(matches) < maxSuggestions && !slices.Contains(matches, m.name) {
			matches = append(matches, m.name)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("channel not found: %s", name)
	}
	return nil, fmt.Errorf("channel not found: %s (did you mean: %s)", name, strings.Join(matches, ", "))
}

// suggestionRank reports whether ch is a "Did you mean" suggestion for name
// (a case-insensitive substring of its name or display name) and its rank,
// lower first: 0 for an exact name or display name, 1 for a prefix of the name or
// display name, 2 for any other substring.
func suggestionRank(name string, ch *model.Channel) (int, bool) {
	needle := strings.ToLower(name)
	chName, display := strings.ToLower(ch.Name), strings.ToLower(ch.DisplayName)
	switch {
	case chName == needle || display == needle:
		return 0, true
	case strings.HasPrefix(chName, needle) || strings.HasPrefix(display, needle):
		return 1, true
	case strings.Contains(chName, needle) || strings.Contains(display, needle):
		return 2, true
	}
	return 0, false
}

func (c *Context) cachedChannel(key string) (*model.Channel, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.channels[key]
	if !ok || !c.now().Before(e.expires) {
		return nil, false
	}
	return e.val, true
}

func (c *Context) storeChannels(chans ...*model.Channel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := c.now().Add(CacheTTL)
	for _, ch := range chans {
		e := cached[*model.Channel]{val: ch, expires: expires}
		c.channels["id:"+ch.Id] = e
		c.channels["name:"+ch.TeamId+":"+ch.Name] = e
	}
}

// Usernames maps user ids to usernames, fetching unknown or expired ids in
// one request and caching them for CacheTTL. Empty and duplicate ids are
// ignored; ids the server does not return are absent from the result. On a
// failed request the result still holds the cached names, next to the error.
func (c *Context) Usernames(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	var missing []string
	c.mu.Lock()
	now := c.now()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, done := out[id]; done || slices.Contains(missing, id) {
			continue
		}
		if e, ok := c.users[id]; ok && now.Before(e.expires) {
			out[id] = e.val
			continue
		}
		missing = append(missing, id)
	}
	c.mu.Unlock()
	if len(missing) == 0 {
		return out, nil
	}

	users, _, err := c.client.GetUsersByIds(ctx, missing)
	if err != nil {
		return out, WrapErr("/api/v4/users/ids", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	expires := c.now().Add(CacheTTL)
	for _, u := range users {
		if u == nil {
			continue
		}
		c.users[u.Id] = cached[string]{val: u.Username, expires: expires}
		if slices.Contains(missing, u.Id) {
			out[u.Id] = u.Username
		}
	}
	return out, nil
}

// diagLog receives diagnostics that do not fail a tool call.
var diagLog io.Writer = os.Stderr

// AuthorNames is Usernames for rendering post authors and DM members: a failed
// lookup is logged to stderr and yields the cached names only, so the other
// users are shown by user id. Only a canceled ctx fails the call.
func (c *Context) AuthorNames(ctx context.Context, ids []string) (map[string]string, error) {
	names, err := c.Usernames(ctx, ids)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		_, _ = fmt.Fprintf(diagLog, "mm-mcp: users shown by id: %v\n", err)
	}
	return names, nil
}

// errEmptyResponse is a 2xx response whose body decoded to nothing (a literal
// `null`), which a real Mattermost server never sends.
var errEmptyResponse = errors.New("mattermost sent an empty response")
