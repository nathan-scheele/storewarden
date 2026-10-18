package tools

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// shopifyQueryCostLimit is the most a single Admin API query may cost.
const shopifyQueryCostLimit = 1000

// The fixtures are recorded from the dev store by scripts/record-fixture.sh,
// and the handler tests in cmd/storewarden serve them.
const fixtures = "../../cmd/storewarden/testdata/"

// Each Tool's documented maximum cost must be what Shopify reported for the
// Tool's query with the inputs that cost the most, and under Shopify's limit.
func TestEachToolsMaximumQueryCost(t *testing.T) {
	tests := []struct {
		tool    string
		query   string
		maxCost float64
		fixture string // recorded with the inputs that cost the most
	}{
		{"get_shop", getShopQuery, getShopMaxCost, "shop"},
		{"search_products", searchProductsQuery, searchProductsMaxCost, "search_products_last_page"}, // first = 50
		{"get_product", getProductQuery, getProductMaxCost, "get_product"},
		{"get_inventory", getInventoryQuery, getInventoryMaxCost, "get_inventory"},
		{"set_inventory_quantity (dry run)", getAvailableQuantityQuery, getAvailableQuantityMaxCost, "set_inventory_quantity_current"},
		{"set_inventory_quantity (real write)", setAvailableQuantityMutation, setAvailableQuantityMaxCost, "set_inventory_quantity_applied"},
	}
	for _, tt := range tests {
		recordedQuery, err := os.ReadFile(fixtures + tt.fixture + ".graphql")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(recordedQuery)) != tt.query {
			t.Errorf("%s: %s.graphql is not the Tool's query; re-record it", tt.tool, tt.fixture)
			continue
		}
		response, err := os.ReadFile(fixtures + tt.fixture + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var recorded struct {
			Extensions struct {
				Cost struct {
					RequestedQueryCost float64 `json:"requestedQueryCost"`
				} `json:"cost"`
			} `json:"extensions"`
		}
		if err := json.Unmarshal(response, &recorded); err != nil {
			t.Fatal(err)
		}
		if got := recorded.Extensions.Cost.RequestedQueryCost; got != tt.maxCost {
			t.Errorf("%s: Shopify reported a requested cost of %v, documented maximum is %v", tt.tool, got, tt.maxCost)
		}
		if tt.maxCost > shopifyQueryCostLimit {
			t.Errorf("%s: maximum cost %v is over Shopify's limit of %d", tt.tool, tt.maxCost, shopifyQueryCostLimit)
		}
	}
}

// Access scopes stop short of customers and orders, and so do the queries.
func TestNoToolQueriesCustomersOrOrders(t *testing.T) {
	for _, query := range []string{getShopQuery, searchProductsQuery, getProductQuery, getInventoryQuery, getAvailableQuantityQuery, setAvailableQuantityMutation} {
		lower := strings.ToLower(query)
		if strings.Contains(lower, "customer") || strings.Contains(lower, "order") {
			t.Errorf("query mentions customers or orders:\n%s", query)
		}
	}
}
