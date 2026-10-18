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

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
	"tailscale.com/tailcfg/peercap"

	"github.com/nathan-scheele/storewarden/internal/audit"
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

// Audit returns the Caller as Audit records show it.
func (c *Caller) Audit() audit.Caller {
	return audit.Caller{
		NodeID:   string(c.NodeID),
		NodeName: c.NodeName,
		Tags:     c.Tags,
		User:     c.User,
	}
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

// HTTP returns middleware that gives every request a new audit request ID,
// identifies it with WhoIs, and puts both in the request context. A request
// whose Caller can't be identified, or has no storewarden capability, gets
// HTTP 403 and a denial Audit record.
// tools maps the name of every storewarden Tool to whether this server
// serves it: a Write tool isn't served unless Write mode is on. A Caller is
// only ever allowed Tools that are served. A Grant naming a Tool that isn't
// in tools at all is logged as a warning.
func HTTP(whois WhoIser, tools map[string]bool, log *audit.Log, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := audit.WithRequestID(r.Context(), audit.NewRequestID())
		caller, err := identify(ctx, whois, r.RemoteAddr, tools, logger)
		if err != nil {
			logger.Info("request denied", "remote_addr", r.RemoteAddr, "reason", err.Error())
			// The request is refused either way, so a failed write needs
			// no handling here: audit.Log logs it and turns unhealthy.
			log.Write(audit.Record{
				RequestID: audit.RequestID(ctx),
				Phase:     audit.Denied,
				Caller:    caller.Audit(),
				Decision:  audit.Deny,
				Reason:    err.Error(),
			})
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, callerKey{}, caller)))
	})
}

// identify returns the Caller behind remoteAddr. When it denies the Caller,
// it still returns whatever identity it learned, for the Audit record.
func identify(ctx context.Context, whois WhoIser, remoteAddr string, tools map[string]bool, logger *slog.Logger) (*Caller, error) {
	who, err := whois.WhoIs(ctx, remoteAddr)
	if err != nil {
		return &Caller{}, fmt.Errorf("whois: %w", err)
	}
	if who.Node == nil {
		return &Caller{}, fmt.Errorf("whois: no node")
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
		return caller, fmt.Errorf("no %s capability", Capability)
	}
	values, err := tailcfg.UnmarshalCapJSON[grantValue](who.CapMap, Capability)
	if err != nil {
		callerLog.Error("invalid grant value in tailnet policy", "capability", Capability, "error", err)
		return caller, fmt.Errorf("invalid grant value: %w", err)
	}

	// Every matching Grant's value arrives in one list; the Caller gets the
	// union of their Tools.
	caller.allowed = map[string]bool{}
	for _, v := range values {
		for _, name := range v.Tools {
			served, known := tools[name]
			if !known {
				callerLog.Warn("grant names an unknown tool", "tool", name)
				continue
			}
			// A Grant for a Write tool is correct policy even while Write
			// mode is off, so it isn't warned about, but nor is it allowed.
			if served {
				caller.allowed[name] = true
			}
		}
	}
	return caller, nil
}

// MCP returns receiving middleware that hides the Tools a Caller isn't
// granted from tools/list, and answers tools/call for them exactly as if the
// Tool didn't exist, with a denial Audit record. tools/call is checked on its
// own, so it doesn't rely on the Caller having listed Tools first.
func MCP(log *audit.Log) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			caller, ok := CallerFrom(ctx)
			if !ok {
				return nil, fmt.Errorf("no caller in context")
			}
			switch method {
			case "tools/call":
				params := req.GetParams().(*mcp.CallToolParamsRaw)
				if !caller.Allowed(params.Name) {
					// Refused either way, as in HTTP.
					log.Write(audit.Record{
						RequestID: audit.RequestID(ctx),
						Phase:     audit.Denied,
						Caller:    caller.Audit(),
						Tool:      params.Name,
						Args:      params.Arguments,
						Decision:  audit.Deny,
						Reason:    "tool not granted",
					})
					return nil, unknownTool(params.Name)
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
