package mattermost

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

func newTestContext(t *testing.T) (*Context, *testutil.Server) {
	t.Helper()
	fake := testutil.New(t)
	c := NewContext(config.Config{
		URL:         fake.URL,
		Token:       testutil.Token,
		Team:        testutil.TeamName,
		DownloadDir: "/dl",
		UploadRoot:  "/up",
	})
	c.me, c.team = fake.Me, fake.Team // as if Init had run, without its requests
	return c, fake
}

func TestClientHeaders(t *testing.T) {
	c, fake := newTestContext(t)
	if err := c.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	h := reqs[0].Header
	if got := h.Get("Accept-Language"); got != "en" {
		t.Errorf("Accept-Language = %q", got)
	}
	if got := h.Get("Authorization"); !strings.EqualFold(got, "Bearer "+testutil.Token) {
		t.Errorf("Authorization header is not a Bearer token")
	}
}

func TestClientErrorNormalization(t *testing.T) {
	tests := []struct {
		name  string
		write func(w http.ResponseWriter)
		want  string
	}{
		{"app error", func(w http.ResponseWriter) {
			testutil.WriteError(w, http.StatusForbidden, "api.x", "No permission.")
		}, "mattermost API 403 /api/v4/users/me: No permission."},
		{"app error without status_code", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"id":"x","message":"Bad thing."}`))
		}, "mattermost API 400 /api/v4/users/me: Bad thing."},
		{"html proxy page", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html><body>Bad Gateway</body></html>"))
		}, "mattermost API 502 /api/v4/users/me: the server sent no message"},
		{"empty body", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
		}, "mattermost API 500 /api/v4/users/me: the server sent no message"},
		{"plain text", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte("http: request body too large\n"))
		}, "mattermost API 413 /api/v4/users/me: http: request body too large"},
		{"multi-line plain text", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("line one\nline two\n"))
		}, "mattermost API 503 /api/v4/users/me: the server sent no message"},
		{"json array", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`["x"]`))
		}, "mattermost API 400 /api/v4/users/me: the server sent no message"},
		{"app error with numeric id", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":404,"message":"Not here."}`))
		}, "mattermost API 404 /api/v4/users/me: the server sent no message"},
		{"app error with object message", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"x","message":{"text":"Not here."}}`))
		}, "mattermost API 404 /api/v4/users/me: the server sent no message"},
		{"app error with fractional status_code", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"x","message":"Not here.","status_code":404.5}`))
		}, "mattermost API 404 /api/v4/users/me: the server sent no message"},
		{"3xx without Location", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusMultipleChoices)
			_, _ = w.Write([]byte("<html>" + strings.Repeat("x", 1<<16) + "</html>"))
		}, "mattermost API 300 /api/v4/users/me: the server sent no message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, fake := newTestContext(t)
			fake.Handle(http.MethodGet, "/users/me", func(w http.ResponseWriter, _ *http.Request) { tt.write(w) })
			err := c.Init(t.Context())
			if _, ok := errors.AsType[*APIError](err); !ok {
				t.Fatalf("not *APIError: %v", err)
			}
			if err.Error() != tt.want {
				t.Fatalf("got %q, want %q", err.Error(), tt.want)
			}
		})
	}
}

func TestTokenNotInErrors(t *testing.T) {
	c := NewContext(config.Config{URL: "http://127.0.0.1:1", Token: testutil.Token, Team: "t"})
	err := c.Init(t.Context())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), testutil.Token) {
		t.Fatal("token leaked into error")
	}
}

func TestClientRedirects(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		testutil.WriteJSON(w, http.StatusOK, map[string]string{"id": testutil.MeID})
	}))
	t.Cleanup(other.Close)

	t.Run("another host is refused", func(t *testing.T) {
		c, fake := newTestContext(t)
		fake.Handle(http.MethodGet, "/users/me", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/api/v4/users/me", http.StatusFound)
		})
		err := c.Init(t.Context())
		if err == nil || !strings.Contains(err.Error(), "refusing to follow it") {
			t.Fatalf("err = %v, want a refused redirect", err)
		}
		if strings.Contains(err.Error(), testutil.Token) {
			t.Fatal("token leaked into error")
		}
		if leaked.Load() {
			t.Fatal("Authorization sent to another host")
		}
	})

	t.Run("same host is followed", func(t *testing.T) {
		c, fake := newTestContext(t)
		fake.Handle(http.MethodGet, "/users/me", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/api/v4/users/"+testutil.MeID, http.StatusFound)
		})
		c.me = nil
		err := c.Init(t.Context())
		if err != nil || c.Me().Id != testutil.MeID {
			t.Fatalf("me = %v, err = %v", c.Me(), err)
		}
	})

	t.Run("method-changing redirect is refused", func(t *testing.T) {
		c, fake := newTestContext(t)
		fake.Handle(http.MethodDelete, "/posts/{id}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, r.URL.Path, http.StatusMovedPermanently)
		})
		_, err := c.Client().DeletePost(t.Context(), "p1")
		if err == nil || !strings.Contains(err.Error(), "MM_MCP_URL") {
			t.Fatalf("err = %v, want a refused redirect naming MM_MCP_URL", err)
		}
		for _, r := range fake.Requests() {
			if r.Method != http.MethodDelete {
				t.Fatalf("redirect followed as %s %s", r.Method, r.Path)
			}
		}
	})

	t.Run("method-keeping redirect is followed", func(t *testing.T) {
		c, fake := newTestContext(t)
		fake.Handle(http.MethodDelete, "/posts/{id}", func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("id") == "p1" {
				http.Redirect(w, r, "/api/v4/posts/p2", http.StatusPermanentRedirect)
				return
			}
			testutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "OK"})
		})
		if _, err := c.Client().DeletePost(t.Context(), "p1"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("policy", func(t *testing.T) {
		req := func(raw string) *http.Request {
			r, err := http.NewRequest(http.MethodGet, raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		tests := []struct {
			from, to string
			ok       bool
		}{
			{"https://mm.example.com/a", "https://mm.example.com/b", true},
			{"http://mm.example.com/a", "https://mm.example.com/b", true},
			{"https://mm.example.com/a", "http://mm.example.com/b", false},
			{"https://mm.example.com/a", "https://evil.mm.example.com/b", false},
			{"https://mm.example.com/a", "https://mm.example.com:8443/b", false},
		}
		for _, tt := range tests {
			err := sameOrigin(req(tt.to), []*http.Request{req(tt.from)})
			if (err == nil) != tt.ok {
				t.Errorf("%s -> %s: err = %v, want ok=%v", tt.from, tt.to, err, tt.ok)
			}
		}
		del := req("https://mm.example.com/a")
		del.Method = http.MethodDelete
		if sameOrigin(req("https://mm.example.com/a"), []*http.Request{del}) == nil {
			t.Error("DELETE turned into GET was followed")
		}
		via := make([]*http.Request, maxRedirects)
		for i := range via {
			via[i] = req("https://mm.example.com/a")
		}
		if sameOrigin(req("https://mm.example.com/b"), via) == nil {
			t.Error("redirect loop not stopped")
		}
	})
}
