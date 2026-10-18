package main

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/authz"
	"github.com/nathan-scheele/storewarden/internal/shopify"
	"github.com/nathan-scheele/storewarden/internal/tools"
)

// newHandler assembles storewarden's HTTP handler: the MCP endpoint behind
// WhoIs authorization, and the unauthenticated health endpoint.
func newHandler(whois authz.WhoIser, client *shopify.Client, logger *slog.Logger) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "storewarden", Version: version}, &mcp.ServerOptions{Logger: logger})
	toolNames := tools.Register(server, client)
	server.AddReceivingMiddleware(authz.MCP())

	// Stateless, so every POST is identified and authorized on its own
	// (docs/adr/0001-stateless-mcp-for-per-request-identity.md).
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, Logger: logger},
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", authz.HTTP(whois, toolNames, logger, mcpHandler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "healthy")
	})
	return mux
}
