// Package tools defines storewarden's Tools: fixed operations on the Store,
// each backed by one fixed GraphQL document.
package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// readOnly annotates a Read tool.
var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

// destructive annotates a Write tool. DestructiveHint is a *bool, so that
// "unset" differs from false; new(true) makes a *bool that points at true.
var destructive = &mcp.ToolAnnotations{DestructiveHint: new(true), IdempotentHint: true}

// Register adds the Tools to s: the Read tools always, and the Write tool
// only in Write mode. It returns every Tool's name, mapped to whether it
// was added.
func Register(s *mcp.Server, client *shopify.Client, writeMode bool) map[string]bool {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_shop",
		Description: "Get the Store's name, myshopify domain, plan and currency, and Shopify's current rate limit status.",
		Annotations: readOnly,
	}, getShop(client))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_products",
		Description: "Search the Store's products with Shopify's search syntax, one page at a time. Returns each product's ID, title, handle, status and total inventory, and a cursor for the next page.",
		Annotations: readOnly,
	}, searchProducts(client))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_product",
		Description: "Get one product by ID: its title, handle, status, tags, and its first 100 variants with their inventory item IDs.",
		Annotations: readOnly,
	}, getProduct(client))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_inventory",
		Description: "Get an inventory item's available and on-hand quantities at each location that stocks it, up to 100 locations.",
		Annotations: readOnly,
	}, getInventory(client))
	if writeMode {
		mcp.AddTool(s, &mcp.Tool{
			Name:        setInventoryQuantityName,
			Description: setInventoryQuantityDescription,
			Annotations: destructive,
		}, setInventoryQuantity(client))
	}
	return map[string]bool{
		"get_shop":               true,
		"search_products":        true,
		"get_product":            true,
		"get_inventory":          true,
		setInventoryQuantityName: writeMode,
	}
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
}

// ThrottleStatus is the state of the Store's query cost bucket, as get_shop
// reports it. It is separate from shopify.ThrottleStatus so the Tool's output
// doesn't change if Shopify's wire format does.
type ThrottleStatus struct {
	CurrentlyAvailable float64 `json:"currently_available"`
	MaximumAvailable   float64 `json:"maximum_available"`
	RestoreRate        float64 `json:"restore_rate" jsonschema:"points restored per second"`
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
		return nil, out, nil
	}
}
