package rudderstackai

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPResourceNamesNormalizedOnce(t *testing.T) {
	for _, path := range []string{"list-then-read", "read-only"} {
		t.Run(path, func(t *testing.T) {
			template := "memo://" + strings.Repeat("a", 309) + "/{id}"
			staticName := strings.Repeat("x", 255) + "\u0085tail"
			s := mcp.NewServer(&mcp.Implementation{Name: "resource-normalization-" + path, Version: "1"}, nil)
			read := func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "body"}}}, nil
			}
			s.AddResource(&mcp.Resource{Name: staticName, URI: "config://opaque"}, read)
			s.AddResourceTemplate(&mcp.ResourceTemplate{Name: "long-template", URITemplate: template}, read)
			r := wrap(t, s, Options{})
			cs := connect(t, s, "2025-06-18", nil)
			listing := cs
			if path == "read-only" {
				// Another client populates the process-wide cache; cs only reads.
				listing = connect(t, s, "2025-06-18", nil)
			}
			ctx := context.Background()
			if _, err := listing.ListResources(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := listing.ListResourceTemplates(ctx, nil); err != nil {
				t.Fatal(err)
			}
			// Repeated reads also exercise reuse of the normalized cache.
			for range 2 {
				for _, uri := range []string{"config://opaque", strings.Replace(template, "{id}", "42", 1)} {
					if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if utf8.RuneCountInString(template) != 321 {
				t.Fatal("template must have 321 code points")
			}
			reads := r.named("mcp_resource_read")
			if len(reads) != 4 {
				t.Fatalf("got %d reads, want 4", len(reads))
			}
			want := []string{strings.Repeat("x", 255) + "\u0085", template[:256]}
			for i, event := range reads {
				got, _ := event.Properties["resource_name"].(string)
				if got != want[i%2] || utf8.RuneCountInString(got) != 256 {
					t.Errorf("read %d resource_name = %q (%d code points), want %q", i, got, utf8.RuneCountInString(got), want[i%2])
				}
			}
		})
	}
}
