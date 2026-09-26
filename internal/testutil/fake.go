package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
)

// APIPrefix is the path prefix of every Mattermost REST route; Handle patterns are relative to it.
const APIPrefix = "/api/v4"

// Request is one recorded HTTP request as the fake received it.
type Request struct {
	Method string
	Path   string // full URL path, e.g. "/api/v4/users/me"
	Query  url.Values
	Header http.Header
	Body   []byte
}

// Server is a fake Mattermost instance backed by httptest.Server.
//
// Fixtures are seeded by New and may be mutated by a test before it issues requests;
// the seeded handlers read them on every call.
type Server struct {
	URL      string // base URL without /api/v4, suitable for config.Config.URL
	Me       *model.User
	Team     *model.Team
	Users    []*model.User    // includes Me
	Channels []*model.Channel // each has Me as a member; seeded ones are in Team, tests may add others
	Version  string           // X-Version-Id header sent on every response; "" sends none

	mu       sync.Mutex
	entries  []routeEntry
	requests []Request
}

type route struct {
	method   string
	segments []string
	literals int
}

type routeEntry struct {
	route
	fn http.HandlerFunc
}

// New starts a fake server with seeded fixtures and default routes, closed on test cleanup.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{}
	s.seed()
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Client returns a model.Client4 pointed at the fake and authenticated with Token.
func (s *Server) Client() *model.Client4 {
	c := model.NewAPIv4Client(s.URL)
	c.SetToken(Token)
	return c
}

// Handle registers fn for method and pattern. The pattern is a path relative to /api/v4 whose
// segments may be `{name}` wildcards matching one segment; fn reads them with r.PathValue(name).
// The route with the most literal segments wins; on a tie the latest registration wins, so a
// test can override any seeded route.
func (s *Server) Handle(method, pattern string, fn http.HandlerFunc) {
	segs := strings.Split(strings.Trim(pattern, "/"), "/")
	lit := 0
	for _, seg := range segs {
		if !isWildcard(seg) {
			lit++
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, routeEntry{route{method, segs, lit}, fn})
}

// HandleClientConfig registers fn for GET /config/client. Like servers before v11, the route
// answers 501 unless the request carries format=old.
func (s *Server) HandleClientConfig(fn http.HandlerFunc) {
	s.Handle(http.MethodGet, "/config/client", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "old" {
			WriteError(w, http.StatusNotImplemented, "api.config.client.old_format.app_error",
				"New format for the client configuration is not supported yet. Please specify format=old in the query string.")
			return
		}
		fn(w, r)
	})
}

// Requests returns a copy of every request received so far, in arrival order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// ResetRequests forgets the requests received so far, e.g. those of setup.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a Mattermost AppError body (id, message, detailed_error, request_id,
// status_code), which model.Client4 decodes into *model.AppError.
func WriteError(w http.ResponseWriter, status int, id, message string) {
	WriteJSON(w, status, &model.AppError{
		Id:         id,
		Message:    message,
		RequestId:  "fakerequest000000000000000",
		StatusCode: status,
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Header: r.Header.Clone(),
		Body:   body,
	})
	fn := s.match(r)
	s.mu.Unlock()

	if s.Version != "" {
		w.Header().Set(model.HeaderVersionId, s.Version)
	}
	if fn == nil {
		WriteError(w, http.StatusNotFound, "api.context.404.app_error", "Sorry, we could not find the page.")
		return
	}
	auth := r.Header.Get("Authorization")
	if len(auth) < 7 || !strings.EqualFold(auth[:7], "bearer ") || auth[7:] != Token {
		WriteError(w, http.StatusUnauthorized, "api.context.session_expired.app_error",
			"Invalid or expired session, please login again.")
		return
	}
	fn(w, r)
}

// match finds the best route for r and sets its path values; the caller holds s.mu.
func (s *Server) match(r *http.Request) http.HandlerFunc {
	rest, ok := strings.CutPrefix(r.URL.Path, APIPrefix+"/")
	if !ok {
		return nil
	}
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	var best *routeEntry
	for i := range s.entries {
		e := &s.entries[i]
		if e.method != r.Method || len(e.segments) != len(segs) {
			continue
		}
		if best != nil && e.literals < best.literals {
			continue
		}
		if matchSegments(e.segments, segs) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	for i, seg := range best.segments {
		if isWildcard(seg) {
			r.SetPathValue(seg[1:len(seg)-1], segs[i])
		}
	}
	return best.fn
}

func matchSegments(pattern, segs []string) bool {
	for i, p := range pattern {
		if !isWildcard(p) && p != segs[i] {
			return false
		}
	}
	return true
}

func isWildcard(seg string) bool {
	return len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}'
}
