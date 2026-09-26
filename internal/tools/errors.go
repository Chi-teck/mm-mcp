package tools

import (
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Chi-teck/mm-mcp/internal/mattermost"
)

// errorText renders err as one line: its whole text,
// wrapping context included, with runs of whitespace collapsed.
func errorText(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

// errorResult turns err into a tool result with isError set; it is
// never a JSON-RPC error.
func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: errorText(err)}},
		IsError: true,
	}
}

// sizePair renders a size and the limit it exceeds for an error message, switching both to
// exact byte counts when their human forms match ("100.0 MB, over the 100.0 MB limit").
func sizePair(size, limit int64) (string, string) {
	s, l := mattermost.HumanSize(size), mattermost.HumanSize(limit)
	if s == l {
		return fmt.Sprintf("%d bytes", size), fmt.Sprintf("%d bytes", limit)
	}
	return s, l
}
