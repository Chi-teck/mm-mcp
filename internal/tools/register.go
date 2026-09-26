package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/mattermost"
)

// hints are the MCP annotations of one tool. destructive and idempotent
// are only meaningful when readOnly is false.
type hints struct {
	readOnly, destructive, idempotent bool
}

// annotations is the table of tool annotations. addTool refuses any tool not listed here.
var annotations = map[string]hints{
	"list_channels":   {readOnly: true},
	"read_posts":      {readOnly: true},
	"get_post":        {readOnly: true},
	"search":          {readOnly: true},
	"list_members":    {readOnly: true},
	"get_file":        {},
	"follow_thread":   {idempotent: true},
	"unfollow_thread": {idempotent: true},
	"create_post":     {},
	"react":           {destructive: true, idempotent: true},
	"edit_post":       {destructive: true},
	"dm":              {idempotent: true},
	"api":             {destructive: true},
}

// toolAnnotations converts the table entry for name into MCP annotations;
// openWorldHint is true for every tool.
func toolAnnotations(name string) *mcp.ToolAnnotations {
	h, ok := annotations[name]
	if !ok {
		panic(fmt.Sprintf("tools: %q is not in the annotation table", name))
	}
	a := &mcp.ToolAnnotations{ReadOnlyHint: h.readOnly, OpenWorldHint: new(true)}
	if !h.readOnly {
		a.DestructiveHint = new(h.destructive)
		a.IdempotentHint = h.idempotent
	}
	return a
}

// handler is what every tool implements: typed, schema-validated input in,
// text out. A returned error becomes an isError result.
type handler[In any] func(ctx context.Context, in In) (string, error)

// schemaEdit adjusts the input schema inferred from a tool's input struct,
// for constraints the jsonschema tag cannot express (the tag is only a
// description).
type schemaEdit func(*jsonschema.Schema)

// limitRange limits the integer property "limit" to [1, maxLimit]. Out-of-range
// values are refused by schema validation before the handler runs, never clamped.
func limitRange(s *jsonschema.Schema) {
	p, ok := s.Properties["limit"]
	if !ok {
		panic("tools: limitRange: no property \"limit\"")
	}
	p.Minimum, p.Maximum = new(1.0), new(float64(maxLimit))
}

// defaultTo sets the schema default of the optional property prop to v. go-sdk
// fills an omitted property with it before the handler runs, so handlers never
// see the zero value.
func defaultTo(prop string, v any) schemaEdit {
	return func(s *jsonschema.Schema) {
		p, ok := s.Properties[prop]
		if !ok {
			panic(fmt.Sprintf("tools: defaultTo: no property %q", prop))
		}
		raw, err := json.Marshal(v)
		if err != nil {
			panic(fmt.Sprintf("tools: defaultTo %s: %v", prop, err))
		}
		p.Default = raw
	}
}

// panicLog receives the panic value and stack of a crashed handler.
var panicLog io.Writer = os.Stderr

// addTool registers the tool name on s. The input schema is inferred from In
// (`json` tags name the properties, jsonschema tags describe them, omitempty
// marks them optional) and then adjusted by edits. It panics if name is not
// in the annotation table or the schema cannot be built, so the table and the
// code cannot drift.
func addTool[In any](s *mcp.Server, name, description string, h handler[In], edits ...schemaEdit) {
	annotations := toolAnnotations(name)
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("tools: %s: input schema: %v", name, err))
	}
	for _, edit := range edits {
		edit(schema)
	}
	tool := &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: annotations,
		InputSchema: schema,
	}
	mcp.AddTool(s, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, _ any, _ error) {
		// Neither go-sdk nor the stdio loop recovers, so a panic here would end
		// the process mid-call.
		defer func() {
			if v := recover(); v != nil {
				_, _ = fmt.Fprintf(panicLog, "mm-mcp: panic in %s: %v\n%s", name, v, debug.Stack())
				res = errorResult(fmt.Errorf("internal error in %s; details are in the server log", name))
			}
		}()
		text, err := h(ctx, in)
		if err != nil {
			return errorResult(err), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}

// Register adds every tool to server. It is the single place where tools
// are added.
func Register(server *mcp.Server, c *mattermost.Context) {
	registerRead(server, c)
	registerMisc(server, c)
	registerWrite(server, c)
	registerFiles(server, c)
	registerAPI(server, c)
}
