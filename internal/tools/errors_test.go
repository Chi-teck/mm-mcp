package tools

import (
	"errors"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/mattermost"
)

func TestErrorText(t *testing.T) {
	apiErr := &mattermost.APIError{Status: 403, Path: "/api/v4/posts", Message: "You do not have permission."}
	want403 := "mattermost API 403 /api/v4/posts: You do not have permission."
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"api error", apiErr, want403},
		{"wrapped api error", fmt.Errorf("create post: %w", apiErr), "create post: " + want403},
		{"wrapped orphan error", fmt.Errorf("post: %w", orphaned([]string{"f1", "f2"}, "the post", apiErr)),
			"post: uploaded 2 files, then the post failed — file ids f1, f2 " + orphanNote + "; cause: " + want403},
		{"no server message", &mattermost.APIError{Status: 502, Path: "/api/v4/users/me"},
			"mattermost API 502 /api/v4/users/me: " + mattermost.NoServerMessage},
		{"plain error", errors.New("channel not found: foo"), "channel not found: foo"},
		{"multi-line error", errors.New("first line\n  second line"), "first line second line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorText(tt.err); got != tt.want {
				t.Errorf("errorText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestErrorResult(t *testing.T) {
	res := errorResult(errors.New("boom"))
	if !res.IsError {
		t.Error("IsError = false, want true")
	}
	if len(res.Content) != 1 {
		t.Fatalf("got %d content items, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "boom" {
		t.Errorf("content = %#v, want text %q", res.Content[0], "boom")
	}
}

func TestSizePair(t *testing.T) {
	cases := []struct {
		size, limit int64
		wantSize    string
		wantLimit   string
	}{
		{200, 100, "200 B", "100 B"},
		{300 << 20, 256 << 20, "300.0 MB", "256.0 MB"},
		{100<<20 + 1, 100 << 20, "104857601 bytes", "104857600 bytes"},
	}
	for _, c := range cases {
		size, limit := sizePair(c.size, c.limit)
		if size != c.wantSize || limit != c.wantLimit {
			t.Errorf("sizePair(%d, %d) = %q, %q; want %q, %q", c.size, c.limit, size, limit, c.wantSize, c.wantLimit)
		}
	}
}
