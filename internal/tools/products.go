package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// searchProductsQuery has a maximum query cost of 9, at first = 50.
const searchProductsQuery = `query SearchProducts($query: String!, $first: Int!, $after: String) {
  products(query: $query, first: $first, after: $after) {
    nodes {
      id
      title
      handle
      status
      totalInventory
    }
    pageInfo {
      hasNextPage
      endCursor
    }
  }
}`

// search_products' page size: the default, and the largest a Caller may ask
// for. searchProductsQuery's maximum cost depends on the largest.
const (
	defaultPageSize = 10
	maxPageSize     = 50
)

// SearchProductsInput is search_products' input.
type SearchProductsInput struct {
	Query string `json:"query,omitempty" jsonschema:"Shopify product search syntax, such as title:snowboard or tag:winter status:active. Empty matches every product."`
	First *int   `json:"first,omitempty" jsonschema:"page size, from 1 to 50; the default is 10"`
	After string `json:"after,omitempty" jsonschema:"next_cursor from the previous page, to get the page after it"`
}

// ProductSearch is search_products' output.
type ProductSearch struct {
	Products   []ProductSummary `json:"products"`
	NextCursor string           `json:"next_cursor,omitempty" jsonschema:"pass as after to get the next page; absent on the last page"`
}

// ProductSummary is one product in a search result.
type ProductSummary struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Handle         string `json:"handle"`
	Status         string `json:"status"`
	TotalInventory int    `json:"total_inventory"`
}

func searchProducts(client *shopify.Client) mcp.ToolHandlerFor[SearchProductsInput, ProductSearch] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SearchProductsInput) (*mcp.CallToolResult, ProductSearch, error) {
		var data struct {
			Products struct {
				Nodes []struct {
					ID             string `json:"id"`
					Title          string `json:"title"`
					Handle         string `json:"handle"`
					Status         string `json:"status"`
					TotalInventory int    `json:"totalInventory"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"products"`
		}
		first := defaultPageSize
		if in.First != nil {
			first = *in.First
		}
		if first < 1 || first > maxPageSize {
			return nil, ProductSearch{}, fmt.Errorf("first must be from 1 to %d, not %d", maxPageSize, first)
		}
		// Shopify wants null, not "", for the first page.
		var after any
		if in.After != "" {
			after = in.After
		}
		variables := map[string]any{"query": in.Query, "first": first, "after": after}
		if _, err := client.Query(ctx, searchProductsQuery, variables, &data); err != nil {
			return nil, ProductSearch{}, err
		}
		out := ProductSearch{Products: []ProductSummary{}}
		for _, p := range data.Products.Nodes {
			out.Products = append(out.Products, ProductSummary{
				ID:             p.ID,
				Title:          p.Title,
				Handle:         p.Handle,
				Status:         p.Status,
				TotalInventory: p.TotalInventory,
			})
		}
		// Shopify gives an end cursor on the last page too, but it leads nowhere.
		if data.Products.PageInfo.HasNextPage {
			out.NextCursor = data.Products.PageInfo.EndCursor
		}
		return nil, out, nil
	}
}
