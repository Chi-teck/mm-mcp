package testutil

import (
	"encoding/json"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
)

// Seeded fixture identifiers. Ids are valid 26-char Mattermost ids.
const (
	Token = "faketoken00000000000000000" // the only token the fake accepts

	MeID       = "fakemeuser0000000000000000"
	MeUsername = "alice"
	OwnerID    = "fakeowner00000000000000000"
	OwnerName  = "ivan.ch"
	OtherID    = "fakebob0000000000000000000"
	OtherName  = "bob"

	TeamID   = "faketeam000000000000000000"
	TeamName = "fake-team"

	TownSquareID   = "faketownsquare000000000000"
	TownSquareName = "town-square"
	TestChannelID  = "fakemmtest0000000000000000"
	TestChannel    = "mm-test"
	PrivateID      = "fakedevops0000000000000000"
	PrivateName    = "dev-ops"
	DMID           = "fakedm00000000000000000000"
	DMName         = MeID + "__" + OwnerID // model.GetDMNameFromIds(MeID, OwnerID)

	Version = "10.0.0.123.abc.false" // X-Version-Id: <X.Y.Z>.<build>.<hash>.<licensed>
)

func (s *Server) seed() {
	s.Version = Version
	s.Me = &model.User{Id: MeID, Username: MeUsername, Locale: "en", Roles: model.SystemUserRoleId}
	s.Users = []*model.User{
		s.Me,
		{Id: OwnerID, Username: OwnerName, FirstName: "Ivan", Locale: "en"},
		{Id: OtherID, Username: OtherName, FirstName: "Bob", Locale: "en"},
	}
	s.Team = &model.Team{Id: TeamID, Name: TeamName, DisplayName: "Fake Team", Type: model.TeamOpen}
	s.Channels = []*model.Channel{
		{Id: TownSquareID, TeamId: TeamID, Name: TownSquareName, DisplayName: "Town Square", Type: model.ChannelTypeOpen},
		{Id: TestChannelID, TeamId: TeamID, Name: TestChannel, DisplayName: "MM Test", Type: model.ChannelTypeOpen},
		{Id: PrivateID, TeamId: TeamID, Name: PrivateName, DisplayName: "Dev Ops", Type: model.ChannelTypePrivate},
		{Id: DMID, Name: DMName, Type: model.ChannelTypeDirect},
	}

	s.Handle("GET", "/users/{user_id}", func(w http.ResponseWriter, r *http.Request) {
		s.writeUser(w, s.findUser(func(u *model.User) bool { return u.Id == s.meAlias(r.PathValue("user_id")) }))
	})
	s.Handle("GET", "/users/username/{username}", func(w http.ResponseWriter, r *http.Request) {
		s.writeUser(w, s.findUser(func(u *model.User) bool { return u.Username == r.PathValue("username") }))
	})
	s.Handle("POST", "/users/ids", func(w http.ResponseWriter, r *http.Request) {
		var ids []string
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
			WriteError(w, http.StatusBadRequest, "api.context.invalid_body_param.app_error", "Invalid or missing user_ids in request body.")
			return
		}
		out := make([]*model.User, 0)
		for _, id := range ids {
			if u := s.findUser(func(u *model.User) bool { return u.Id == id }); u != nil {
				out = append(out, u)
			}
		}
		WriteJSON(w, http.StatusOK, out)
	})
	s.Handle("GET", "/teams/{team_id}", func(w http.ResponseWriter, r *http.Request) {
		s.writeTeam(w, r.PathValue("team_id") == s.Team.Id)
	})
	s.Handle("GET", "/teams/name/{team_name}", func(w http.ResponseWriter, r *http.Request) {
		s.writeTeam(w, r.PathValue("team_name") == s.Team.Name)
	})
	s.Handle("GET", "/channels/{channel_id}", func(w http.ResponseWriter, r *http.Request) {
		s.writeChannel(w, s.findChannel(func(c *model.Channel) bool { return c.Id == r.PathValue("channel_id") }))
	})
	s.Handle("GET", "/teams/{team_id}/channels/name/{channel_name}", func(w http.ResponseWriter, r *http.Request) {
		s.writeChannel(w, s.findChannel(func(c *model.Channel) bool {
			// Like Mattermost, archived channels are hidden unless include_deleted=true.
			return c.TeamId == r.PathValue("team_id") && c.Name == r.PathValue("channel_name") &&
				(c.DeleteAt == 0 || r.URL.Query().Get("include_deleted") == "true")
		}))
	})
	s.Handle("GET", "/users/{user_id}/teams/{team_id}/channels", func(w http.ResponseWriter, r *http.Request) {
		if s.meAlias(r.PathValue("user_id")) != s.Me.Id || r.PathValue("team_id") != s.Team.Id {
			WriteError(w, http.StatusForbidden, "api.context.permissions.app_error", "You do not have the appropriate permissions.")
			return
		}
		// Mattermost includes DM and group channels in this team listing.
		out := make([]*model.Channel, 0)
		for _, c := range s.Channels {
			if (c.TeamId == s.Team.Id || c.TeamId == "") && (c.DeleteAt == 0 || r.URL.Query().Get("include_deleted") == "true") {
				out = append(out, c)
			}
		}
		WriteJSON(w, http.StatusOK, out)
	})
}

func (s *Server) meAlias(id string) string {
	if id == "me" {
		return s.Me.Id
	}
	return id
}

func (s *Server) findUser(pred func(*model.User) bool) *model.User {
	for _, u := range s.Users {
		if pred(u) {
			return u
		}
	}
	return nil
}

func (s *Server) findChannel(pred func(*model.Channel) bool) *model.Channel {
	for _, c := range s.Channels {
		if pred(c) {
			return c
		}
	}
	return nil
}

func (*Server) writeUser(w http.ResponseWriter, u *model.User) {
	if u == nil {
		WriteError(w, http.StatusNotFound, "app.user.missing_account.const", "Unable to find the user.")
		return
	}
	WriteJSON(w, http.StatusOK, u)
}

func (s *Server) writeTeam(w http.ResponseWriter, found bool) {
	if !found {
		WriteError(w, http.StatusNotFound, "app.team.get_by_name.missing.app_error", "Unable to find the existing team.")
		return
	}
	WriteJSON(w, http.StatusOK, s.Team)
}

func (*Server) writeChannel(w http.ResponseWriter, c *model.Channel) {
	if c == nil {
		WriteError(w, http.StatusNotFound, "app.channel.get_by_name.missing.app_error", "Channel does not exist.")
		return
	}
	WriteJSON(w, http.StatusOK, c)
}
