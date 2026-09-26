package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/config"
	"github.com/Chi-teck/mm-mcp/internal/mattermost"
	"github.com/Chi-teck/mm-mcp/internal/testutil"
)

// harness is the shared end-to-end test rig for tool files: a fake Mattermost server, a
// *mattermost.Context pointed at it, every tool registered on an MCP server, and an MCP
// client connected to that server over the SDK's in-memory transport.
//
// Typical use:
//
//	h := newHarness(t)
//	h.fake.Handle(http.MethodGet, "/posts/{post_id}", ...) // routes the fake doesn't seed
//	text, isErr := h.callTool(t, "get_post", map[string]any{"post_id": id})
//	text = h.callOK(t, "list_channels", nil) // fails the test on isError
//
// Mutate h.fake fixtures or add handlers before the first call that needs them. Init has already
// cached the current user, the team and the server version; change those through a
// newConfigHarness setup instead.
type harness struct {
	fake    *testutil.Server
	mm      *mattermost.Context
	session *mcp.ClientSession
}

// newHarness starts the fake, builds the Context (URL, testutil.Token, testutil.TeamName) and
// connects a client session; everything is closed on test cleanup. Context.Init has run and its
// requests are cleared from h.fake.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newConfigHarness(t, config.Config{})
}

// newConfigHarness is newHarness with the optional settings of cfg; its URL and Token are
// filled in, and Team when empty. Each setup runs on the fake before Context.Init, e.g. to edit
// fields of fake.Me in place (fake.Users[0] is the same user, so don't replace the pointer).
func newConfigHarness(t *testing.T, cfg config.Config, setup ...func(*testutil.Server)) *harness {
	t.Helper()
	fake := testutil.New(t)
	for _, f := range setup {
		f(fake)
	}
	cfg.URL, cfg.Token, cfg.Team = fake.URL, testutil.Token, cmp.Or(cfg.Team, testutil.TeamName)
	mm := mattermost.NewContext(cfg)
	initContext(t, mm, fake)
	server := mcp.NewServer(&mcp.Implementation{Name: "mm-mcp-test", Version: "test"}, nil)
	Register(server, mm, "test")

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		_ = serverSession.Wait()
	})
	return &harness{fake: fake, mm: mm, session: session}
}

// initContext runs mm.Init against fake, as main does at startup, and forgets its requests.
func initContext(t *testing.T, mm *mattermost.Context, fake *testutil.Server) {
	t.Helper()
	if err := mm.Init(t.Context()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	fake.ResetRequests()
}

// toolSchemas lists the tools and returns their input schemas by tool name.
func (h *harness) toolSchemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	res, err := h.session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	schemas := map[string]*jsonschema.Schema{}
	for _, tool := range res.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		schemas[tool.Name] = &s
	}
	return schemas
}

// checkSchema fails the test unless tool's schema has exactly the required and the (described)
// props properties.
func checkSchema(t *testing.T, schemas map[string]*jsonschema.Schema, tool string, required, props []string) {
	t.Helper()
	s := schemas[tool]
	if s == nil {
		t.Fatalf("%s not listed", tool)
	}
	if !slices.Equal(s.Required, required) {
		t.Errorf("%s required = %v, want %v", tool, s.Required, required)
	}
	if len(s.Properties) != len(props) {
		t.Errorf("%s has %d properties, want %d", tool, len(s.Properties), len(props))
	}
	for _, name := range props {
		if p := s.Properties[name]; p == nil || p.Description == "" {
			t.Errorf("%s.%s missing or undescribed", tool, name)
		}
	}
}

// callTool calls tool name with args (nil = no arguments) and returns the joined text of the
// result and its isError flag. A JSON-RPC (protocol) error fails the test: tools report every
// failure, schema violations included, as an isError result.
func (h *harness) callTool(t *testing.T, name string, args map[string]any) (text string, isError bool) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := h.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: protocol error: %v", name, err)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n"), res.IsError
}

// callOK is callTool that fails the test on an isError result and returns the text.
func (h *harness) callOK(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	text, isError := h.callTool(t, name, args)
	if isError {
		t.Fatalf("unexpected tool error: %s", text)
	}
	return text
}

// wantErr fails the test unless the result is an error whose text equals want.
func wantErr(t *testing.T, text string, isError bool, want string) {
	t.Helper()
	if !isError {
		t.Fatalf("expected tool error %q, got success: %s", want, text)
	}
	if text != want {
		t.Fatalf("error text:\n got %q\nwant %q", text, want)
	}
}

// testPost builds a post in channelID by userID, created `ago` before now.
func testPost(id, channelID, userID, message string, ago time.Duration) *model.Post {
	ms := time.Now().Add(-ago).UnixMilli()
	return &model.Post{Id: id, ChannelId: channelID, UserId: userID, Message: message, CreateAt: ms, UpdateAt: ms}
}

// postList builds a PostList from posts given newest first, as the server orders them.
func postList(posts ...*model.Post) *model.PostList {
	list := model.NewPostList()
	for _, p := range posts {
		list.AddPost(p)
		list.AddOrder(p.Id)
	}
	return list
}

// postID returns a valid 26-char post id derived from tag (padded with zeros).
func postID(tag string) string {
	return (tag + strings.Repeat("0", 26))[:26]
}
