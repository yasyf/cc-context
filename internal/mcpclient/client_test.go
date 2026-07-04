package mcpclient

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTextOf(t *testing.T) {
	tests := []struct {
		name string
		res  *mcp.CallToolResult
		want string
	}{
		{
			name: "empty result",
			res:  &mcp.CallToolResult{},
			want: "",
		},
		{
			name: "single text",
			res: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "hello"},
			}},
			want: "hello",
		},
		{
			name: "multiple texts concatenated",
			res: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "foo"},
				&mcp.TextContent{Text: "bar"},
				&mcp.TextContent{Text: "baz"},
			}},
			want: "foobarbaz",
		},
		{
			name: "non-text content skipped",
			res: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "a"},
				&mcp.ImageContent{Data: []byte{0x1}, MIMEType: "image/png"},
				&mcp.TextContent{Text: "b"},
			}},
			want: "ab",
		},
		{
			name: "only non-text content",
			res: &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.ImageContent{Data: []byte{0x1}, MIMEType: "image/png"},
			}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TextOf(tt.res); got != tt.want {
				t.Errorf("TextOf() = %q, want %q", got, tt.want)
			}
		})
	}
}
