// Package authz identifies each request's Caller with Tailscale WhoIs and
// decides which Tools the Caller may use, from the storewarden capability in
// the tailnet policy file.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
	"tailscale.com/tailcfg/peercap"
)

// Capability is the name Grants use to give Callers storewarden Tools.
const Capability peercap.Cap = "github.com/nathan-scheele/storewarden"

// WhoIser identifies the tailnet node behind a remote address.
// *local.Client implements it.
type WhoIser interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

// Caller is the tailnet node making a request. Its names and tags are kept
// for the record only and never used to authorize.
type Caller struct {
	NodeID   tailcfg.StableNodeID
	NodeName string
	Tags     []string
	User     string // login name, or "tagged" for a tagged node

	allowed map[string]bool
}

// Allowed reports whether the Caller's Grants name the Tool.
func (c *Caller) Allowed(tool string) bool {
	return c.allowed[tool]
}

type callerKey struct{}

// CallerFrom returns the Caller that the HTTP middleware put in ctx.
func CallerFrom(ctx context.Context) (*Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(*Caller)
	return c, ok
}

// grantValue is one Grant's value for Capability.
type grantValue struct {
	Tools []string `json:"tools"`
}

// UnmarshalJSON rejects unknown fields, so that a typo in the policy file
// denies the Caller instead of silently granting nothing.
func (g *grantValue) UnmarshalJSON(b []byte) error {
	// plain has grantValue's fields but not this method, so Decode below
	// doesn't call UnmarshalJSON again.
	type plain grantValue
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode((*plain)(g))
}

// HTTP returns middleware that identifies every request with WhoIs and puts
// the Caller in the request context. A request whose Caller can't be
// identified, or has no storewarden capability, gets HTTP 403.
// tools lists the Tools this server has; a Grant naming any other Tool is
// logged as a warning.
func HTTP(whois WhoIser, tools []string, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := identify(r.Context(), whois, r.RemoteAddr, tools, logger)
		if err != nil {
			logger.Info("request denied", "remote_addr", r.RemoteAddr, "reason", err.Error())
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, caller)))
	})
}

func identify(ctx context.Context, whois WhoIser, remoteAddr string, tools []string, logger *slog.Logger) (*Caller, error) {
	who, err := whois.WhoIs(ctx, remoteAddr)
	if err != nil {
		return nil, fmt.Errorf("whois: %w", err)
	}
	if who.Node == nil {
		return nil, fmt.Errorf("whois: no node")
	}
	caller := &Caller{
		NodeID:   who.Node.StableID,
		NodeName: who.Node.Name,
	}
	if who.Node.IsTagged() {
		caller.Tags = who.Node.Tags
		caller.User = "tagged"
	} else if who.UserProfile != nil {
		caller.User = who.UserProfile.LoginName
	}
	callerLog := logger.With("node_id", caller.NodeID, "node_name", caller.NodeName)

	if !who.CapMap.HasCapability(Capability) {
		return nil, fmt.Errorf("no %s capability", Capability)
	}
	values, err := tailcfg.UnmarshalCapJSON[grantValue](who.CapMap, Capability)
	if err != nil {
		callerLog.Error("invalid grant value in tailnet policy", "capability", Capability, "error", err)
		return nil, fmt.Errorf("invalid grant value: %w", err)
	}

	// Every matching Grant's value arrives in one list; the Caller gets the
	// union of their Tools.
	caller.allowed = map[string]bool{}
	for _, v := range values {
		for _, name := range v.Tools {
			if !slices.Contains(tools, name) {
				callerLog.Warn("grant names an unknown tool", "tool", name)
				continue
			}
			caller.allowed[name] = true
		}
	}
	return caller, nil
}

// MCP returns receiving middleware that hides the Tools a Caller isn't
// granted from tools/list, and answers tools/call for them exactly as if the
// Tool didn't exist. tools/call is checked on its own, so it doesn't rely on
// the Caller having listed Tools first.
func MCP() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			caller, ok := CallerFrom(ctx)
			if !ok {
				return nil, fmt.Errorf("no caller in context")
			}
			switch method {
			case "tools/call":
				name := req.GetParams().(*mcp.CallToolParamsRaw).Name
				if !caller.Allowed(name) {
					return nil, unknownTool(name)
				}
			case "tools/list":
				res, err := next(ctx, method, req)
				if err != nil {
					return nil, err
				}
				list, ok := res.(*mcp.ListToolsResult)
				if !ok {
					return nil, fmt.Errorf("tools/list returned %T", res)
				}
				granted := []*mcp.Tool{}
				for _, t := range list.Tools {
					if caller.Allowed(t.Name) {
						granted = append(granted, t)
					}
				}
				list.Tools = granted
				// The list now depends on who asked, so only this
				// Caller's own client may cache it.
				list.CacheScope = "private"
				return list, nil
			}
			return next(ctx, method, req)
		}
	}
}

// unknownTool is the error the MCP SDK returns for a Tool that doesn't exist.
func unknownTool(name string) error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInvalidParams,
		Message: fmt.Sprintf("unknown tool %q", name),
	}
}
