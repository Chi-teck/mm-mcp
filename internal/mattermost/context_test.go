package mattermost

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

func newFakeContext(t *testing.T) (*testutil.Server, *Context) {
	t.Helper()
	s := testutil.New(t)
	c := NewContext(config.Config{URL: s.URL, Token: testutil.Token, Team: testutil.TeamName})
	c.me, c.team = s.Me, s.Team // as if Init had run, without its requests
	return s, c
}

// fakeClock is a settable clock for cache TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func withClock(c *Context) *fakeClock {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c.now = clk.now
	return clk
}

func countRequests(fake *testutil.Server, method, path string) int {
	n := 0
	for _, r := range fake.Requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

func TestNewContextNoIO(t *testing.T) {
	c, fake := newTestContext(t)
	if n := len(fake.Requests()); n != 0 {
		t.Fatalf("constructor sent %d requests", n)
	}
	if c.URL() != fake.URL || c.TeamName() != testutil.TeamName || c.DownloadDir() != "/dl" || c.UploadRoot() != "/up" {
		t.Fatal("config accessors mismatch")
	}
	if c.Client() == nil {
		t.Fatal("nil client")
	}
}

func TestInit(t *testing.T) {
	c, fake := newTestContext(t)
	c.me, c.team = nil, nil
	if err := c.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	me, team := c.Me(), c.Team()
	if me == nil || team == nil || me.Id != testutil.MeID || team.Id != testutil.TeamID {
		t.Fatalf("me=%v team=%v", me, team)
	}
	if n := len(fake.Requests()); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
}

func TestInitBadTeam(t *testing.T) {
	for _, ref := range []string{"nope", "nosuchteam0000000000000000"} {
		c, _ := newTestContext(t)
		c.cfg.Team = ref
		err := c.Init(t.Context())
		if want := "mattermost team not found: " + ref; err == nil || err.Error() != want {
			t.Fatalf("got %v, want %q", err, want)
		}
	}
}

// Client4 strips ".." from path segments, so "mm..test" would load team mmtest.
func TestInitTeamDotDot(t *testing.T) {
	c, fake := newTestContext(t)
	c.cfg.Team = "mm..test"
	err := c.Init(t.Context())
	if want := `invalid team: "mm..test" (".." is not allowed)`; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if n := len(fake.Requests()); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
}

func TestInitTeamByID(t *testing.T) {
	c, fake := newTestContext(t)
	c.cfg.Team = testutil.TeamID
	if err := c.Init(t.Context()); err != nil || c.Team().Id != testutil.TeamID {
		t.Fatalf("got %v, %v", c.Team(), err)
	}
	if countRequests(fake, http.MethodGet, "/api/v4/teams/"+testutil.TeamID) != 1 {
		t.Fatal("team not fetched by id")
	}
}

func TestInitTeamServerErrorNotRelabeled(t *testing.T) {
	c, fake := newTestContext(t)
	fake.Handle(http.MethodGet, "/teams/name/{team_name}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusInternalServerError, "x", "db down")
	})
	err := c.Init(t.Context())
	want := "mattermost API 500 /api/v4/teams/name/" + testutil.TeamName + ": db down"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// TestInitNullResponse: a `null` me or team fails startup clearly instead of
// leaving a nil for the tools to dereference.
func TestInitNullResponse(t *testing.T) {
	for _, pattern := range []string{"/users/me", "/teams/name/{team_name}"} {
		t.Run(pattern, func(t *testing.T) {
			c, fake := newTestContext(t)
			fake.Handle(http.MethodGet, pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("null"))
			})
			if err := c.Init(t.Context()); err == nil || err.Error() != "mattermost sent an empty response" {
				t.Fatalf("got %v; want the empty-response error", err)
			}
		})
	}
}

func TestIsID(t *testing.T) {
	for s, want := range map[string]bool{
		testutil.TownSquareID:        true,
		"town-square":                false,
		"FAKETOWNSQUARE000000000000": false,
		testutil.TownSquareID + "0":  false,
		"fake_ownsquare000000000000": false,
		"abcdefghijklmnopqrstuvwxyz": true,
	} {
		if got := IsID(s); got != want {
			t.Errorf("IsID(%q) = %v", s, got)
		}
	}
}

func TestResolveChannel(t *testing.T) {
	tests := []struct{ in, wantID string }{
		{testutil.TestChannel, testutil.TestChannelID},
		{testutil.TestChannelID, testutil.TestChannelID},
		{testutil.PrivateName, testutil.PrivateID},
		{testutil.DMName, testutil.DMID},
	}
	for _, tt := range tests {
		c, _ := newTestContext(t)
		ch, err := c.ResolveChannel(t.Context(), tt.in)
		if err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}
		if ch.Id != tt.wantID {
			t.Fatalf("%s: id = %s", tt.in, ch.Id)
		}
	}
}

func TestResolveChannelCache(t *testing.T) {
	c, fake := newTestContext(t)
	clk := withClock(c)
	byName := "/api/v4/teams/" + testutil.TeamID + "/channels/name/" + testutil.TestChannel
	byID := "/api/v4/channels/" + testutil.TestChannelID
	resolve := func(s string) {
		t.Helper()
		if _, err := c.ResolveChannel(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	resolve(testutil.TestChannel)
	resolve(testutil.TestChannel)
	resolve(testutil.TestChannelID) // cached under its id too
	if countRequests(fake, http.MethodGet, byName) != 1 || countRequests(fake, http.MethodGet, byID) != 0 {
		t.Fatal("cache miss within TTL")
	}
	clk.advance(CacheTTL)
	resolve(testutil.TestChannelID)
	resolve(testutil.TestChannel)
	if countRequests(fake, http.MethodGet, byName) != 1 || countRequests(fake, http.MethodGet, byID) != 1 {
		t.Fatal("expected one refetch by id after TTL, then a name hit")
	}
}

// An archived channel resolves by name on a cold cache, as it does by id.
func TestResolveChannelArchivedByName(t *testing.T) {
	c, fake := newTestContext(t)
	const archivedID = "archivedchannel00000000000"
	fake.Channels = append(fake.Channels, &model.Channel{
		Id: archivedID, TeamId: testutil.TeamID, Name: "old-stuff", Type: model.ChannelTypeOpen, DeleteAt: 1,
	})
	ch, err := c.ResolveChannel(t.Context(), "old-stuff")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Id != archivedID {
		t.Fatalf("id = %s, want %s", ch.Id, archivedID)
	}
}

func TestResolveChannelCacheIgnoresOtherTeams(t *testing.T) {
	c, fake := newTestContext(t)
	const otherID = "othertownsquare00000000000"
	fake.Channels = append(fake.Channels, &model.Channel{
		Id: otherID, TeamId: "otherteam00000000000000000", Name: testutil.TownSquareName, Type: model.ChannelTypeOpen,
	})
	if _, err := c.ResolveChannel(t.Context(), otherID); err != nil { // e.g. get_post in another team
		t.Fatal(err)
	}
	ch, err := c.ResolveChannel(t.Context(), testutil.TownSquareName)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Id != testutil.TownSquareID {
		t.Fatalf("id = %s, want the configured team's %s", ch.Id, testutil.TownSquareID)
	}
}

func TestResolveChannelCacheDM(t *testing.T) {
	c, fake := newTestContext(t)
	for range 2 {
		if _, err := c.ResolveChannel(t.Context(), testutil.DMName); err != nil {
			t.Fatal(err)
		}
	}
	mine := "/api/v4/users/" + testutil.MeID + "/teams/" + testutil.TeamID + "/channels"
	if n := countRequests(fake, http.MethodGet, mine); n != 1 {
		t.Fatalf("my channels fetched %d times, want 1", n)
	}
}

func TestResolveChannelNotFound(t *testing.T) {
	tests := []struct{ in, want string }{
		{"dev", "channel not found: dev (did you mean: dev-ops)"},
		{"TEST", "channel not found: TEST (did you mean: mm-test)"},
		{"zzz", "channel not found: zzz"},
	}
	for _, tt := range tests {
		c, _ := newTestContext(t)
		_, err := c.ResolveChannel(t.Context(), tt.in)
		if err == nil || err.Error() != tt.want {
			t.Errorf("%s: got %v, want %q", tt.in, err, tt.want)
		}
	}
}

func TestResolveChannelDotsRefused(t *testing.T) {
	c, fake := newTestContext(t)
	fake.Channels = append(fake.Channels, &model.Channel{
		Id: "mmtest0000000000000000000x", TeamId: testutil.TeamID, Name: "mmtest", Type: model.ChannelTypeOpen,
	})
	_, err := c.ResolveChannel(t.Context(), "mm..test") // Client4 would strip ".." and find mmtest
	if want := `invalid channel name: "mm..test" (".." is not allowed)`; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if n := len(fake.Requests()); n != 0 {
		t.Fatalf("%d requests sent, want 0", n)
	}
}

func TestResolveChannelIDNotFound(t *testing.T) {
	c, _ := newTestContext(t)
	const id = "nosuchchannel0000000000000"
	_, err := c.ResolveChannel(t.Context(), id)
	if want := "channel not found: " + id; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestResolveChannelIDShapedName(t *testing.T) {
	c, fake := newTestContext(t)
	const id, name = "anonchannel000000000000000", "0123456789abcdefghijklmnop" // UseAnonymousURLs-style name
	fake.Channels = append(fake.Channels, &model.Channel{
		Id: id, TeamId: testutil.TeamID, Name: name, Type: model.ChannelTypeOpen,
	})
	ch, err := c.ResolveChannel(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Id != id {
		t.Fatalf("id = %s, want %s", ch.Id, id)
	}
	if countRequests(fake, http.MethodGet, "/api/v4/channels/"+name) != 1 {
		t.Fatal("expected the id lookup first")
	}
}

func TestResolveChannelEmpty(t *testing.T) {
	for _, in := range []string{"", "  \t"} {
		c, fake := newTestContext(t)
		_, err := c.ResolveChannel(t.Context(), in)
		if want := "channel is required (a name or id)"; err == nil || err.Error() != want {
			t.Fatalf("%q: got %v, want %q", in, err, want)
		}
		if n := len(fake.Requests()); n != 0 {
			t.Fatalf("%q: %d requests sent, want 0", in, n)
		}
	}
}

func TestResolveChannelIDNotFoundMalformedBody(t *testing.T) {
	for _, body := range []string{
		`{"id":404}`,
		`{"message":{"text":"gone"}}`,
		`{"status_code":404.5}`,
	} {
		t.Run(body, func(t *testing.T) {
			c, fake := newTestContext(t)
			fake.Handle(http.MethodGet, "/channels/{channel_id}", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(body))
			})
			_, err := c.ResolveChannel(t.Context(), testutil.PrivateID)
			if want := "channel not found: " + testutil.PrivateID; err == nil || err.Error() != want {
				t.Fatalf("got %v, want %q", err, want)
			}
		})
	}
}

func TestResolveChannelIDForbidden(t *testing.T) {
	c, fake := newTestContext(t)
	fake.Handle(http.MethodGet, "/channels/{channel_id}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusForbidden, "x", "No permission.")
	})
	_, err := c.ResolveChannel(t.Context(), testutil.PrivateID)
	want := "mattermost API 403 /api/v4/channels/" + testutil.PrivateID + ": No permission."
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestResolveChannelSuggestionsByDisplayName(t *testing.T) {
	c, fake := newTestContext(t)
	for _, ch := range fake.Channels {
		if ch.Id == testutil.TownSquareID {
			ch.DisplayName = "Backend Devs"
		}
	}
	_, err := c.ResolveChannel(t.Context(), "dev")
	want := "channel not found: dev (did you mean: dev-ops, town-square)"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestResolveChannelServerError(t *testing.T) {
	c, fake := newTestContext(t)
	fake.Handle(http.MethodGet, "/teams/{team_id}/channels/name/{channel_name}", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusInternalServerError, "x", "db down")
	})
	_, err := c.ResolveChannel(t.Context(), "dev")
	want := "mattermost API 500 /api/v4/teams/" + testutil.TeamID + "/channels/name/dev: db down"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v", err)
	}
}

func TestUsernames(t *testing.T) {
	c, fake := newTestContext(t)
	clk := withClock(c)
	got, err := c.Usernames(t.Context(), []string{testutil.OwnerID, "", testutil.OtherID, testutil.OwnerID, "unknownuser000000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{testutil.OwnerID: testutil.OwnerName, testutil.OtherID: testutil.OtherName}
	if !maps.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
	got, err = c.Usernames(t.Context(), []string{testutil.OtherID})
	if err != nil || got[testutil.OtherID] != testutil.OtherName {
		t.Fatalf("got %v, %v", got, err)
	}
	if n := countRequests(fake, http.MethodPost, "/api/v4/users/ids"); n != 1 {
		t.Fatalf("requests = %d, want 1 (cached)", n)
	}
	clk.advance(CacheTTL)
	if _, err := c.Usernames(t.Context(), []string{testutil.OtherID}); err != nil {
		t.Fatal(err)
	}
	if n := countRequests(fake, http.MethodPost, "/api/v4/users/ids"); n != 2 {
		t.Fatalf("requests = %d, want 2 after TTL", n)
	}
}

func TestUsernamesError(t *testing.T) {
	c, fake := newTestContext(t)
	fake.Handle(http.MethodPost, "/users/ids", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusBadRequest, "x", "bad ids")
	})
	_, err := c.Usernames(t.Context(), []string{testutil.OtherID})
	if err == nil || err.Error() != "mattermost API 400 /api/v4/users/ids: bad ids" {
		t.Fatalf("got %v", err)
	}
}

func TestUsernamesNullUser(t *testing.T) {
	c, fake := newTestContext(t)
	fake.Handle(http.MethodPost, "/users/ids", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[null]`))
	})
	got, err := c.Usernames(t.Context(), []string{testutil.OtherID})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestAuthorNamesDegrades(t *testing.T) {
	c, fake := newTestContext(t)
	var log strings.Builder
	old := diagLog
	diagLog = &log
	t.Cleanup(func() { diagLog = old })
	if _, err := c.Usernames(t.Context(), []string{testutil.OwnerID}); err != nil {
		t.Fatal(err)
	}
	fake.Handle(http.MethodPost, "/users/ids", func(w http.ResponseWriter, _ *http.Request) {
		testutil.WriteError(w, http.StatusInternalServerError, "x", "db down")
	})
	got, err := c.AuthorNames(t.Context(), []string{testutil.OwnerID, testutil.OtherID})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{testutil.OwnerID: testutil.OwnerName}; !maps.Equal(got, want) {
		t.Fatalf("got %v, want the cached name only", got)
	}
	if want := "mm-mcp: users shown by id: mattermost API 500 /api/v4/users/ids: db down\n"; log.String() != want {
		t.Fatalf("log = %q, want %q", log.String(), want)
	}
	if strings.Contains(log.String(), testutil.Token) {
		t.Fatal("token logged")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.AuthorNames(ctx, []string{testutil.OtherID}); err == nil {
		t.Fatal("canceled lookup succeeded")
	}
}

func TestResolveChannelSuggestionsRanked(t *testing.T) {
	c, fake := newTestContext(t)
	// Five alphabetically-earlier substring matches would push the prefix
	// and exact display-name matches out of an alphabetical top 5.
	for i, name := range []string{"a-ops", "b-ops", "c-ops", "d-ops", "e-ops", "ops-team", "zz-room"} {
		display := name
		if name == "zz-room" {
			display = "OPS"
		}
		fake.Channels = append(fake.Channels, &model.Channel{
			Id: fmt.Sprintf("ops%023d", i), TeamId: testutil.TeamID, Name: name,
			DisplayName: display, Type: model.ChannelTypeOpen,
		})
	}
	_, err := c.ResolveChannel(t.Context(), "ops")
	want := "channel not found: ops (did you mean: zz-room, ops-team, a-ops, b-ops, c-ops)"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}
