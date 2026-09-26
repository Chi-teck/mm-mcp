package testutil_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

func TestFixtureIDs(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9]{26}$`)
	for _, id := range []string{
		testutil.Token, testutil.MeID, testutil.OwnerID, testutil.OtherID, testutil.TeamID,
		testutil.TownSquareID, testutil.TestChannelID, testutil.PrivateID, testutil.DMID,
	} {
		if !re.MatchString(id) || !model.IsValidId(id) {
			t.Errorf("invalid id %q", id)
		}
	}
	if got := model.GetDMNameFromIds(testutil.MeID, testutil.OwnerID); got != testutil.DMName {
		t.Errorf("DMName = %q, want %q", testutil.DMName, got)
	}
}

func TestSeededRoutes(t *testing.T) {
	s := testutil.New(t)
	c := s.Client()
	ctx := context.Background()

	me, _, err := c.GetMe(ctx, "")
	if err != nil || me.Id != testutil.MeID {
		t.Fatalf("GetMe = %v, %v", me, err)
	}
	u, _, err := c.GetUserByUsername(ctx, testutil.OwnerName, "")
	if err != nil || u.Id != testutil.OwnerID {
		t.Fatalf("GetUserByUsername = %v, %v", u, err)
	}
	users, _, err := c.GetUsersByIds(ctx, []string{testutil.OtherID, "nope"})
	if err != nil || len(users) != 1 || users[0].Username != testutil.OtherName {
		t.Fatalf("GetUsersByIds = %v, %v", users, err)
	}
	team, _, err := c.GetTeamByName(ctx, testutil.TeamName, "")
	if err != nil || team.Id != testutil.TeamID {
		t.Fatalf("GetTeamByName = %v, %v", team, err)
	}
	ch, _, err := c.GetChannelByName(ctx, testutil.TestChannel, testutil.TeamID, "")
	if err != nil || ch.Id != testutil.TestChannelID {
		t.Fatalf("GetChannelByName = %v, %v", ch, err)
	}
	ch, _, err = c.GetChannel(ctx, testutil.DMID)
	if err != nil || ch.Name != testutil.DMName {
		t.Fatalf("GetChannel = %v, %v", ch, err)
	}
	chs, _, err := c.GetChannelsForTeamForUser(ctx, testutil.TeamID, "me", false, "")
	if err != nil || len(chs) != len(s.Channels) {
		t.Fatalf("GetChannelsForTeamForUser = %d channels, %v", len(chs), err)
	}
}

func TestErrorDecodesToAppError(t *testing.T) {
	s := testutil.New(t)
	_, resp, err := s.Client().GetChannelByName(context.Background(), "missing", testutil.TeamID, "")
	var appErr *model.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %T %v, want *model.AppError", err, err)
	}
	if appErr.StatusCode != http.StatusNotFound || resp.StatusCode != http.StatusNotFound ||
		appErr.Id != "app.channel.get_by_name.missing.app_error" || appErr.Message != "Channel does not exist." {
		t.Errorf("appErr = %+v", appErr)
	}
}

func TestUnknownRouteAndBadToken(t *testing.T) {
	s := testutil.New(t)
	ctx := context.Background()
	_, _, err := s.Client().GetPost(ctx, testutil.MeID, "")
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.StatusCode != http.StatusNotFound || appErr.Id != "api.context.404.app_error" {
		t.Errorf("unknown route err = %v", err)
	}

	c := model.NewAPIv4Client(s.URL)
	c.SetToken("wrong")
	_, _, err = c.GetMe(ctx, "")
	if !errors.As(err, &appErr) || appErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token err = %v", err)
	}
}

func TestHandleOverrideAndRecording(t *testing.T) {
	s := testutil.New(t)
	s.Handle("GET", "/users/{user_id}", func(w http.ResponseWriter, r *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, &model.User{Id: r.PathValue("user_id"), Username: "override"})
	})
	s.Handle("PUT", "/posts/{post_id}/patch", func(w http.ResponseWriter, r *http.Request) {
		testutil.WriteJSON(w, http.StatusOK, &model.Post{Id: r.PathValue("post_id"), Message: "edited"})
	})
	c := s.Client()
	ctx := context.Background()

	u, _, err := c.GetUser(ctx, testutil.OtherID, "")
	if err != nil || u.Username != "override" || u.Id != testutil.OtherID {
		t.Fatalf("overridden GetUser = %v, %v", u, err)
	}
	if _, _, err := c.PatchPost(ctx, "post1", &model.PostPatch{Message: new("edited")}); err != nil {
		t.Fatalf("PatchPost: %v", err)
	}
	if _, _, err := c.GetUserByUsername(ctx, testutil.MeUsername, ""); err != nil {
		t.Fatalf("GetUserByUsername after override: %v", err)
	}

	reqs := s.Requests()
	if len(reqs) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(reqs))
	}
	r := reqs[1]
	if r.Method != "PUT" || r.Path != "/api/v4/posts/post1/patch" || !regexp.MustCompile(`"message":"edited"`).Match(r.Body) {
		t.Errorf("recorded %s %s %s", r.Method, r.Path, r.Body)
	}
	if got := r.Header.Get("Authorization"); got != "BEARER "+testutil.Token && got != "Bearer "+testutil.Token {
		t.Errorf("Authorization = %q", got)
	}
}

func TestQueryRecorded(t *testing.T) {
	s := testutil.New(t)
	_, _, _ = s.Client().GetChannelsForTeamForUser(context.Background(), testutil.TeamID, testutil.MeID, true, "")
	if q := s.Requests()[0].Query; q.Get("include_deleted") != "true" {
		t.Errorf("query = %v", q)
	}
}

func TestRoutePrecedence(t *testing.T) {
	s := testutil.New(t)
	named := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			testutil.WriteJSON(w, http.StatusOK, &model.User{Username: name})
		}
	}
	// Same segment count: a literal registered before a wildcard still wins.
	s.Handle("GET", "/users/{user_id}/image", named("image"))
	s.Handle("GET", "/users/{user_id}/{sub}", named("wild"))
	s.Handle("GET", "/users/{user_id}/{other}", named("wild-latest"))
	ctx := context.Background()
	for path, want := range map[string]string{
		"/users/x/image": "image",
		"/users/x/stats": "wild-latest", // equal literal count: latest registration wins
	} {
		var u model.User
		resp, err := s.Client().DoAPIGet(ctx, path, "")
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if err := jsonDecode(resp, &u); err != nil || u.Username != want {
			t.Errorf("GET %s = %q, %v; want %q", path, u.Username, err, want)
		}
	}
}

func TestSeededNotFoundAndForbidden(t *testing.T) {
	s := testutil.New(t)
	c := s.Client()
	ctx := context.Background()
	check := func(name string, err error, status int) {
		t.Helper()
		if appErr, ok := errors.AsType[*model.AppError](err); !ok || appErr.StatusCode != status || appErr.Message == "" {
			t.Errorf("%s: err = %v, want AppError %d", name, err, status)
		}
	}
	_, _, err := c.GetUser(ctx, "nouser00000000000000000000", "")
	check("GetUser", err, http.StatusNotFound)
	_, _, err = c.GetUserByUsername(ctx, "nobody", "")
	check("GetUserByUsername", err, http.StatusNotFound)
	_, _, err = c.GetTeamByName(ctx, "other-team", "")
	check("GetTeamByName", err, http.StatusNotFound)
	_, _, err = c.GetTeam(ctx, "noteam00000000000000000000", "")
	check("GetTeam", err, http.StatusNotFound)
	_, _, err = c.GetChannel(ctx, "nochannel00000000000000000")
	check("GetChannel", err, http.StatusNotFound)
	_, _, err = c.GetChannelsForTeamForUser(ctx, testutil.TeamID, testutil.OtherID, false, "")
	check("GetChannelsForTeamForUser other user", err, http.StatusForbidden)
	resp, err := c.DoAPIPost(ctx, "/users/ids", "not json")
	if err == nil {
		_ = resp.Body.Close()
	}
	check("POST /users/ids bad body", err, http.StatusBadRequest)
}

func TestConcurrentUse(t *testing.T) {
	s := testutil.New(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, _, err := s.Client().GetMe(ctx, ""); err != nil {
				t.Errorf("GetMe: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			s.Handle("GET", "/posts/{post_id}", func(w http.ResponseWriter, r *http.Request) {
				testutil.WriteJSON(w, http.StatusOK, &model.Post{Id: r.PathValue("post_id"), Message: string('a' + rune(i))})
			})
			if _, _, err := s.Client().GetPost(ctx, "post1", ""); err != nil {
				t.Errorf("GetPost: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := len(s.Requests()); n != 16 {
		t.Errorf("recorded %d requests, want 16", n)
	}
}

func jsonDecode(resp *http.Response, v any) error {
	err := json.NewDecoder(resp.Body).Decode(v)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	return err
}
