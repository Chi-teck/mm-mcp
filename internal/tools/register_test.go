package tools

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/mattermost"
	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

func TestAnnotationTable(t *testing.T) {
	// In table order: name, readOnly, destructive, idempotent.
	want := []struct {
		name                              string
		readOnly, destructive, idempotent bool
	}{
		{"list_channels", true, false, false},
		{"read_posts", true, false, false},
		{"get_post", true, false, false},
		{"search", true, false, false},
		{"list_members", true, false, false},
		{"get_file", false, false, false},
		{"follow_thread", false, false, true},
		{"unfollow_thread", false, false, true},
		{"create_post", false, false, false},
		{"react", false, true, true},
		{"edit_post", false, true, false},
		{"dm", false, false, true},
		{"api", false, true, false},
	}
	if len(annotations) != len(want) {
		t.Errorf("annotation table has %d tools, want %d", len(annotations), len(want))
	}
	for _, w := range want {
		a := toolAnnotations(w.name)
		if a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("%s: openWorldHint not true", w.name)
		}
		if a.ReadOnlyHint != w.readOnly {
			t.Errorf("%s: readOnlyHint = %v, want %v", w.name, a.ReadOnlyHint, w.readOnly)
		}
		if w.readOnly {
			if a.DestructiveHint != nil || a.IdempotentHint {
				t.Errorf("%s: read-only tool carries destructive/idempotent hints", w.name)
			}
			continue
		}
		if a.DestructiveHint == nil || *a.DestructiveHint != w.destructive {
			t.Errorf("%s: destructiveHint = %v, want %v", w.name, a.DestructiveHint, w.destructive)
		}
		if a.IdempotentHint != w.idempotent {
			t.Errorf("%s: idempotentHint = %v, want %v", w.name, a.IdempotentHint, w.idempotent)
		}
	}
}

type limitInput struct {
	Channel string `json:"channel" jsonschema:"channel name or id"`
	Limit   int    `json:"limit,omitempty" jsonschema:"posts to show"`
}

func newServer() *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
}

func mustPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		msg, _ := r.(string)
		if !strings.Contains(msg, want) {
			t.Errorf("panic = %v, want one containing %q", r, want)
		}
	}()
	f()
}

func TestAddToolUnknownNamePanics(t *testing.T) {
	mustPanic(t, `"no_such_tool" is not in the annotation table`, func() {
		addTool(newServer(), "no_such_tool", "", func(context.Context, limitInput) (string, error) { return "", nil })
	})
}

func TestLimitRangeUnknownPropertyPanics(t *testing.T) {
	mustPanic(t, `no property "limit"`, func() {
		addTool(newServer(), "get_post", "", func(context.Context, getPostIn) (string, error) { return "", nil },
			limitRange)
	})
}

// connect registers one tool through addTool and returns a connected client.
func connect(t *testing.T, h handler[limitInput]) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := newServer()
	addTool(server, "read_posts", "Read posts.", h, limitRange)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	return cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_posts", Arguments: args})
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("got %d content items, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}
	return text.Text
}

func TestAddToolListing(t *testing.T) {
	cs := connect(t, func(context.Context, limitInput) (string, error) { return "", nil })
	list, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(list.Tools))
	}
	tool := list.Tools[0]
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Errorf("annotations = %+v, want readOnlyHint", tool.Annotations)
	}
	if tool.OutputSchema != nil {
		t.Errorf("outputSchema = %v, want none", tool.OutputSchema)
	}
	schema, _ := tool.InputSchema.(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	limit, _ := props["limit"].(map[string]any)
	if limit["minimum"] != 1.0 || limit["maximum"] != 200.0 || limit["description"] != "posts to show" {
		t.Errorf("limit schema = %v, want minimum 1, maximum 200 and a description", limit)
	}
	if schema["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", schema["additionalProperties"])
	}
	required, _ := schema["required"].([]any)
	if !slices.Equal(required, []any{"channel"}) {
		t.Errorf("required = %v, want [channel]", required)
	}
}

func TestAddToolCall(t *testing.T) {
	var got limitInput
	cs := connect(t, func(_ context.Context, in limitInput) (string, error) {
		got = in
		switch in.Channel {
		case "api":
			return "", &mattermost.APIError{Status: 404, Path: "/api/v4/channels/x", Message: "Not found."}
		case "plain":
			return "", errors.New("channel not found: plain")
		}
		return "hello " + in.Channel, nil
	})

	res, err := call(t, cs, map[string]any{"channel": "town-square", "limit": 5})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || resultText(t, res) != "hello town-square" || got.Limit != 5 {
		t.Errorf("ok call: isError=%v text=%q input=%+v", res.IsError, resultText(t, res), got)
	}

	for channel, want := range map[string]string{
		"api":   "mattermost API 404 /api/v4/channels/x: Not found.",
		"plain": "channel not found: plain",
	} {
		res, err := call(t, cs, map[string]any{"channel": channel})
		if err != nil {
			t.Fatalf("%s: protocol error %v, want a tool error result", channel, err)
		}
		if !res.IsError || resultText(t, res) != want {
			t.Errorf("%s: isError=%v text=%q, want isError and %q", channel, res.IsError, resultText(t, res), want)
		}
	}
}

func TestAddToolRefusesOutOfRange(t *testing.T) {
	called := false
	cs := connect(t, func(context.Context, limitInput) (string, error) {
		called = true
		return "", nil
	})
	for _, args := range []map[string]any{
		{"channel": "x", "limit": 201},
		{"channel": "x", "limit": -1},
		{"channel": "x", "extra": true},
		{"channel": "x", "limit": 0},
		{"limit": 5},
	} {
		res, err := call(t, cs, args)
		if err != nil {
			t.Errorf("%v: protocol error %v, want a tool error result", args, err)
		} else if !res.IsError {
			t.Errorf("%v: accepted, want refusal", args)
		}
	}
	if called {
		t.Error("handler ran for invalid input")
	}
}

func TestAddToolRecoversPanic(t *testing.T) {
	var log bytes.Buffer
	old := panicLog
	panicLog = &log
	t.Cleanup(func() { panicLog = old })

	// A 2xx literal `null` body decodes to a nil post that get_post dereferences.
	h := newHarness(t)
	h.fake.Handle(http.MethodGet, "/posts/{post_id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("null"))
	})
	text, isErr := h.callTool(t, "get_post", map[string]any{"post_id": postID("p1")})
	wantErr(t, text, isErr, "internal error in get_post; details are in the server log")
	if got := log.String(); !strings.Contains(got, "mm-mcp: panic in get_post: runtime error") ||
		!strings.Contains(got, "goroutine ") || strings.Contains(got, testutil.Token) {
		t.Fatalf("panic log missing value/stack or leaks the token:\n%s", got)
	}
	// The server is still serving.
	h.callOK(t, "list_channels", nil)
}
