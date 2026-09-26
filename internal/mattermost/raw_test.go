package mattermost

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

func TestRawDo(t *testing.T) {
	s, c := newFakeContext(t)
	s.Handle(http.MethodPost, "/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("got " + string(b) + " " + r.URL.RawQuery))
	})
	resp, err := c.RawDo(context.Background(), http.MethodPost, "/api/v4/echo?a=1", strings.NewReader(`{"x":1}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated || string(b) != `got {"x":1} a=1` {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
	reqs := s.Requests()
	r := reqs[len(reqs)-1]
	if r.Header.Get("Authorization") != "Bearer "+testutil.Token {
		t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("Accept-Language") != "en" {
		t.Errorf("Accept-Language = %q", r.Header.Get("Accept-Language"))
	}
	if r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
	}
}

func TestRawDoNoContentType(t *testing.T) {
	s, c := newFakeContext(t)
	resp, err := c.RawDo(context.Background(), http.MethodGet, "/api/v4/users/me", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	reqs := s.Requests()
	if ct := reqs[len(reqs)-1].Header.Get("Content-Type"); ct != "" {
		t.Fatalf("Content-Type = %q, want none", ct)
	}
}

func TestRawDoErrorBodyUntouched(t *testing.T) {
	s, c := newFakeContext(t)
	s.Handle(http.MethodGet, "/plain", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "proxy says no", http.StatusBadGateway)
	})
	resp, err := c.RawDo(context.Background(), http.MethodGet, "/api/v4/plain", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || string(b) != "proxy says no\n" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
}

func TestRawDoDoesNotFollowRedirect(t *testing.T) {
	s, c := newFakeContext(t)
	s.Handle(http.MethodGet, "/files/{file_id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", s.URL+"/api/v4/users/me") // same host: a followed redirect would show up in Requests
		w.WriteHeader(http.StatusFound)
	})
	resp, err := c.RawDo(context.Background(), http.MethodGet, "/api/v4/files/abc", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != s.URL+"/api/v4/users/me" {
		t.Fatalf("got %d Location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if n := len(s.Requests()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

func TestRawDoContextCanceled(t *testing.T) {
	_, c := newFakeContext(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := c.RawDo(ctx, http.MethodGet, "/api/v4/users/me", nil, "")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRawDoRelativePath(t *testing.T) {
	_, c := newFakeContext(t)
	resp, err := c.RawDo(context.Background(), http.MethodGet, "api/v4/users/me", nil, "")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("want error for a path without leading /")
	}
}

func TestRawDoErrorHidesToken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // connection refused → *url.Error from the transport
	const token = "secret-token-value-xyz"
	c := NewContext(config.Config{URL: "http://" + addr, Token: token, Team: testutil.TeamName})
	resp, err := c.RawDo(context.Background(), http.MethodGet, "/api/v4/users/me", nil, "")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("want a transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaks the token: %v", err)
	}
}
