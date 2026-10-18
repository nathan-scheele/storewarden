package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/audit"
	"github.com/nathan-scheele/storewarden/internal/authz"
	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// errNotAudited refuses a call whose attempt record couldn't be written.
var errNotAudited = errors.New("storewarden can't write its audit log, so the call was refused")

// Audited returns receiving middleware that writes an attempt record before
// every tools/call reaches its Tool, and a result record afterwards. If the
// attempt record can't be written, the call is refused before Shopify is
// called. It runs inside authz.MCP, so it only sees calls the Caller is
// granted; authz records the denials.
//
// Every Tool is audited here, so a Tool needs no audit code of its own:
// Shopify's costs and request ID reach the result record through
// shopify.WithObserver, whichever queries the Tool runs.
func Audited(log *audit.Log) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			caller, ok := authz.CallerFrom(ctx)
			if !ok {
				return nil, errors.New("no caller in context")
			}
			params := req.GetParams().(*mcp.CallToolParamsRaw)
			rec := audit.Record{
				RequestID: audit.RequestID(ctx),
				Phase:     audit.Attempt,
				Caller:    caller.Audit(),
				Tool:      params.Name,
				Args:      params.Arguments,
				Decision:  audit.Allow,
				// DryRun and IdempotencyKey stay empty for Read tools,
				// which have neither.
			}
			if params.Name == setInventoryQuantityName {
				rec.DryRun, rec.IdempotencyKey = writeArgs(params.Arguments)
			}
			if err := log.Write(rec); err != nil {
				return nil, errNotAudited
			}

			var usage shopifyUsage
			start := time.Now()
			res, err := next(shopify.WithObserver(ctx, usage.add), method, req)

			rec.Time = time.Time{} // Write stamps the result's own time
			rec.Phase = audit.Result
			rec.DurationMS = float64(time.Since(start)) / float64(time.Millisecond)
			rec.Shopify = usage.total()
			rec.Outcome, rec.Error = outcome(res, err)
			// A lost result record is logged and makes storewarden
			// unhealthy (audit.Log does both), but the call has already
			// happened, so its result still goes back to the Caller.
			log.Write(rec)
			return res, err
		}
	}
}

// writeArgs returns whether a Write tool call is a Dry run, and the
// Idempotency key it was given, from the arguments the Caller sent. The
// arguments haven't been validated yet; if they can't be read, the call is
// taken as a Dry run, which is what the Tool will treat it as or refuse.
func writeArgs(args json.RawMessage) (dryRun bool, idempotencyKey string) {
	var in struct {
		DryRun         *bool  `json:"dry_run"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return true, ""
	}
	return isDryRun(in.DryRun), in.IdempotencyKey
}

// outcome describes how a call ended, for the result record.
func outcome(res mcp.Result, err error) (string, string) {
	if err != nil {
		return audit.Failed, err.Error()
	}
	if r, ok := res.(*mcp.CallToolResult); ok && r.IsError {
		// A Tool's error comes back as text content (see the SDK's
		// toolForErr).
		var msgs []string
		for _, c := range r.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				msgs = append(msgs, t.Text)
			}
		}
		return audit.Failed, strings.Join(msgs, "; ")
	}
	return audit.OK, ""
}

// shopifyUsage adds up what Shopify reports about the queries one call makes.
type shopifyUsage struct {
	mu  sync.Mutex
	rec *audit.Shopify // nil until Shopify answers a query
}

func (u *shopifyUsage) add(resp *shopify.Response) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.rec == nil {
		u.rec = &audit.Shopify{}
	}
	if resp.Cost != nil {
		u.rec.RequestedCost += resp.Cost.RequestedQueryCost
		u.rec.ActualCost += resp.Cost.ActualQueryCost
	}
	u.rec.RequestID = resp.RequestID
}

func (u *shopifyUsage) total() *audit.Shopify {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.rec
}
