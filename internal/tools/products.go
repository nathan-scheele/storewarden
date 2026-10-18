package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// searchProductsMaxCost is the most searchProductsQuery can cost, when first
// is maxPageSize.
const searchProductsMaxCost = 9

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
	// A pointer, so that a page size of 0 is rejected rather than taken as unset.
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

// getProductMaxCost is the most getProductQuery can cost.
const getProductMaxCost = 21

const getProductQuery = `query GetProduct($id: ID!) {
  product(id: $id) {
    id
    title
    handle
    status
    tags
    variants(first: 100) {
      nodes {
        id
        title
        sku
        inventoryItem {
          id
        }
      }
    }
  }
}`

// GetProductInput is get_product's input.
type GetProductInput struct {
	ID string `json:"id" jsonschema:"the product's ID, such as gid://shopify/Product/123"`
}

// Product is get_product's output.
type Product struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Handle   string    `json:"handle"`
	Status   string    `json:"status"`
	Tags     []string  `json:"tags"`
	Variants []Variant `json:"variants"`
}

// Variant is one of a product's variants.
type Variant struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	SKU             string `json:"sku,omitempty"`
	InventoryItemID string `json:"inventory_item_id" jsonschema:"pass to get_inventory to see this variant's stock"`
}

func getProduct(client *shopify.Client) mcp.ToolHandlerFor[GetProductInput, Product] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetProductInput) (*mcp.CallToolResult, Product, error) {
		if err := checkID("id", "Product", in.ID); err != nil {
			return nil, Product{}, err
		}
		var data struct {
			Product *struct {
				ID       string   `json:"id"`
				Title    string   `json:"title"`
				Handle   string   `json:"handle"`
				Status   string   `json:"status"`
				Tags     []string `json:"tags"`
				Variants struct {
					Nodes []struct {
						ID            string `json:"id"`
						Title         string `json:"title"`
						SKU           string `json:"sku"`
						InventoryItem struct {
							ID string `json:"id"`
						} `json:"inventoryItem"`
					} `json:"nodes"`
				} `json:"variants"`
			} `json:"product"`
		}
		if _, err := client.Query(ctx, getProductQuery, map[string]any{"id": in.ID}, &data); err != nil {
			return nil, Product{}, err
		}
		// Shopify answers null for an ID it doesn't know.
		p := data.Product
		if p == nil {
			return nil, Product{}, fmt.Errorf("no product with ID %s", in.ID)
		}
		out := Product{
			ID:       p.ID,
			Title:    p.Title,
			Handle:   p.Handle,
			Status:   p.Status,
			Tags:     p.Tags,
			Variants: []Variant{},
		}
		for _, v := range p.Variants.Nodes {
			out.Variants = append(out.Variants, Variant{
				ID:              v.ID,
				Title:           v.Title,
				SKU:             v.SKU,
				InventoryItemID: v.InventoryItem.ID,
			})
		}
		return nil, out, nil
	}
}
