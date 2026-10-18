package main

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/audit"
	"github.com/nathan-scheele/storewarden/internal/authz"
	"github.com/nathan-scheele/storewarden/internal/shopify"
	"github.com/nathan-scheele/storewarden/internal/tools"
)

// newHandler assembles storewarden's HTTP handler: the MCP endpoint behind
// WhoIs authorization and auditing, and the unauthenticated health endpoint.
func newHandler(whois authz.WhoIser, client *shopify.Client, auditLog *audit.Log, logger *slog.Logger) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "storewarden", Version: version}, &mcp.ServerOptions{Logger: logger})
	toolNames := tools.Register(server, client)
	// authz.MCP runs first, so tools.Audited only sees granted calls.
	server.AddReceivingMiddleware(authz.MCP(auditLog), tools.Audited(auditLog))

	// Stateless, so every POST is identified and authorized on its own
	// (docs/adr/0001-stateless-mcp-for-per-request-identity.md).
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, Logger: logger},
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", authz.HTTP(whois, toolNames, auditLog, logger, mcpHandler))
	// Unhealthy once an Audit record has been lost.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !auditLog.Healthy() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "healthy")
	})
	return mux
}
