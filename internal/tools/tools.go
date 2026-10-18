// Package tools defines storewarden's Tools: fixed operations on the Store,
// each backed by one fixed GraphQL document.
package tools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// readOnly annotates a Read tool.
var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

// destructive annotates a Write tool. DestructiveHint is a *bool, so that
// "unset" differs from false; new(true) makes a *bool that points at true.
var destructive = &mcp.ToolAnnotations{DestructiveHint: new(true), IdempotentHint: true}

// tool is one of storewarden's Tools.
type tool struct {
	def   *mcp.Tool
	write bool // a Write tool, served only in Write mode
	add   func(s *mcp.Server, client *shopify.Client)
}

// all is every Tool storewarden has, in the order tools/list shows them.
var all = []tool{
	readTool(&mcp.Tool{
		Name:        "get_shop",
		Description: "Get the Store's name, myshopify domain, plan and currency, Shopify's current rate limit status for the Store, and your own Quota of that rate limit.",
	}, getShop),
	readTool(&mcp.Tool{
		Name:        "search_products",
		Description: "Search the Store's products with Shopify's search syntax, one page at a time. Returns each product's ID, title, handle, status and total inventory, and a cursor for the next page.",
	}, searchProducts),
	readTool(&mcp.Tool{
		Name:        "get_product",
		Description: "Get one product by ID: its title, handle, status, tags, and its first 100 variants with their inventory item IDs.",
	}, getProduct),
	readTool(&mcp.Tool{
		Name:        "get_inventory",
		Description: "Get an inventory item's available and on-hand quantities at each location that stocks it, up to 100 locations.",
	}, getInventory),
	writeTool(&mcp.Tool{
		Name:        setInventoryQuantityName,
		Description: setInventoryQuantityDescription,
	}, setInventoryQuantity),
	writeTool(&mcp.Tool{
		Name:        moveInventoryQuantityName,
		Description: moveInventoryQuantityDescription,
	}, moveInventoryQuantity),
}

// readTool makes a Read tool from its definition and the function that
// makes its handler. It is generic because each Tool's handler has its own
// input and output types, from which mcp.AddTool infers the Tool's schemas.
func readTool[In, Out any](def *mcp.Tool, handler func(*shopify.Client) mcp.ToolHandlerFor[In, Out]) tool {
	def.Annotations = readOnly
	return tool{def: def, add: func(s *mcp.Server, client *shopify.Client) {
		mcp.AddTool(s, def, bounded(handler(client)))
	}}
}

// writeTool makes a Write tool, like readTool. Its input must embed
// WriteOptions, which the Audit records show.
func writeTool[In, Out any](def *mcp.Tool, handler func(*shopify.Client) mcp.ToolHandlerFor[In, Out]) tool {
	def.Annotations = destructive
	return tool{def: def, write: true, add: func(s *mcp.Server, client *shopify.Client) {
		mcp.AddTool(s, def, bounded(handler(client)))
	}}
}

// callTimeout is how long one Tool call may take. It is the deadline the
// Shopify client's throttle checks: if waiting for Shopify's rate limit
// would pass it, the call fails at once with a retryable error instead.
const callTimeout = 30 * time.Second

// bounded wraps a Tool's handler so that each call has callTimeout as its
// deadline, and returns the error the Caller should see (see toolError).
func bounded[In, Out any](handler mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		ctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		res, out, err := handler(ctx, req, in)
		return res, out, toolError(err)
	}
}

// Register adds the Tools to s: the Read tools always, and the Write tools
// only in Write mode. It returns every Tool's name, mapped to whether it
// was added.
func Register(s *mcp.Server, client *shopify.Client, writeMode bool) map[string]bool {
	served := map[string]bool{}
	for _, t := range all {
		served[t.def.Name] = !t.write || writeMode
		if served[t.def.Name] {
			t.add(s, client)
		}
	}
	return served
}

// isWriteTool reports whether name is one of the Write tools.
func isWriteTool(name string) bool {
	for _, t := range all {
		if t.def.Name == name {
			return t.write
		}
	}
	return false
}

// getShopMaxCost is the most getShopQuery can cost, in Shopify's query cost
// points. Every Tool's maximum is checked against a recording in cost_test.go
// and passed to the Shopify client, which waits for that much capacity.
const getShopMaxCost = 2

const getShopQuery = `query GetShop {
  shop {
    name
    myshopifyDomain
    currencyCode
    plan { publicDisplayName }
  }
}`

// Shop is get_shop's output.
type Shop struct {
	Name            string          `json:"name"`
	MyshopifyDomain string          `json:"myshopify_domain"`
	Plan            string          `json:"plan"`
	Currency        string          `json:"currency"`
	ThrottleStatus  *ThrottleStatus `json:"throttle_status,omitempty" jsonschema:"Shopify's query cost bucket, if Shopify reported it"`
	Quota           *Quota          `json:"quota,omitempty" jsonschema:"your own share of Shopify's query cost bucket, once Shopify has reported the bucket's limits"`
}

// ThrottleStatus is the state of the Store's query cost bucket, as get_shop
// reports it. It is separate from shopify.ThrottleStatus so the Tool's output
// doesn't change if Shopify's wire format does.
type ThrottleStatus struct {
	CurrentlyAvailable float64 `json:"currently_available"`
	MaximumAvailable   float64 `json:"maximum_available"`
	RestoreRate        float64 `json:"restore_rate" jsonschema:"points restored per second"`
}

// Quota is the state of the calling Caller's own Quota, as get_shop reports
// it, in Shopify's query cost points. Like ThrottleStatus, it is separate
// from shopify.QuotaStatus so the Tool's output is fixed here.
type Quota struct {
	Available   float64 `json:"available"`
	Maximum     float64 `json:"maximum"`
	RestoreRate float64 `json:"restore_rate" jsonschema:"points restored per second"`
}

func getShop(client *shopify.Client) mcp.ToolHandlerFor[struct{}, Shop] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, Shop, error) {
		var data struct {
			Shop struct {
				Name            string `json:"name"`
				MyshopifyDomain string `json:"myshopifyDomain"`
				CurrencyCode    string `json:"currencyCode"`
				Plan            struct {
					PublicDisplayName string `json:"publicDisplayName"`
				} `json:"plan"`
			} `json:"shop"`
		}
		resp, err := client.Query(ctx, getShopQuery, getShopMaxCost, nil, &data)
		if err != nil {
			return nil, Shop{}, err
		}
		out := Shop{
			Name:            data.Shop.Name,
			MyshopifyDomain: data.Shop.MyshopifyDomain,
			Plan:            data.Shop.Plan.PublicDisplayName,
			Currency:        data.Shop.CurrencyCode,
		}
		if resp.Cost != nil {
			t := resp.Cost.ThrottleStatus
			out.ThrottleStatus = &ThrottleStatus{
				CurrentlyAvailable: t.CurrentlyAvailable,
				MaximumAvailable:   t.MaximumAvailable,
				RestoreRate:        t.RestoreRate,
			}
		}
		// After the query, so the Quota shows what this call cost it.
		if q, ok := client.Quota(ctx); ok {
			out.Quota = &Quota{Available: q.Available, Maximum: q.Maximum, RestoreRate: q.RestoreRate}
		}
		return nil, out, nil
	}
}
