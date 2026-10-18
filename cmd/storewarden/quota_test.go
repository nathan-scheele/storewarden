package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

// asNode returns whois's answer, for the node with stable ID id instead.
func asNode(id tailcfg.StableNodeID, whois fakeWhoIs) fakeWhoIs {
	return func() (*apitype.WhoIsResponse, error) {
		resp, err := whois()
		if resp != nil {
			resp.Node.StableID = id
		}
		return resp, err
	}
}

// switchWhoIs answers as whichever node the test chose last, so that one
// server sees calls from several Callers. MCP is stateless, so every call
// is identified afresh.
type switchWhoIs struct {
	mu    sync.Mutex
	whois fakeWhoIs
}

func (s *switchWhoIs) WhoIs(ctx context.Context, addr string) (*apitype.WhoIsResponse, error) {
	s.mu.Lock()
	whois := s.whois
	s.mu.Unlock()
	return whois.WhoIs(ctx, addr)
}

func (s *switchWhoIs) set(whois fakeWhoIs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.whois = whois
}

const getShopGrant = `{"tools":["get_shop"]}`

// spentQuota starts a harness whose Store reports a tiny bucket with a very
// slow refill, and has Caller A spend its Quota. It returns the harness,
// the WhoIs to switch Callers with, and A's session.
func spentQuota(t *testing.T) (*harness, *switchWhoIs, *mcp.ClientSession) {
	t.Helper()
	whois := &switchWhoIs{whois: asNode("nCALLER-A", grantedTo(getShopGrant))}
	h := newHarness(t, whois)
	// get_shop costs at most 2 points. The Store's bucket holds 4 and
	// refills at 0.001 a second, so at the share 0.5 each Quota holds 2 and
	// refills at 0.0005 a second: one more call in 4000s.
	h.store.replaceInResponses(`"maximumAvailable": 20000.0`, `"maximumAvailable": 4.0`)
	h.store.replaceInResponses(`"currentlyAvailable": 19998`, `"currentlyAvailable": 4`)
	h.store.replaceInResponses(`"restoreRate": 1000.0`, `"restoreRate": 0.001`)
	cs := h.connect(t)
	callGetShop(t, cs) // the Store's limits aren't known yet, so this is free
	callGetShop(t, cs) // spends A's Quota
	return h, whois, cs
}

func TestASpentQuotaRefusesOnlyItsCaller(t *testing.T) {
	h, whois, cs := spentQuota(t)

	got := callToolError(t, cs, "get_shop", nil)

	if !strings.HasPrefix(got, "retryable: ") || !strings.Contains(got, "Quota") {
		t.Errorf("error = %q, want a retryable error naming the Caller's Quota", got)
	}
	// 4000s, less whatever the Quota refilled while the test ran.
	if !strings.Contains(got, "Retry the same call in 1h6m") {
		t.Errorf("error = %q, want a suggested wait of about 1h6m40s", got)
	}
	if n := h.store.queries.Load(); n != 2 {
		t.Errorf("Shopify got %d queries, want 2: the refused call never reaches it", n)
	}

	whois.set(asNode("nCALLER-B", grantedTo(getShopGrant)))
	callGetShop(t, cs) // B's Quota is full
}

// A Quota refusal is an allowed call that failed, not a denial: "denied"
// means authorization only.
func TestAQuotaRefusalHasAttemptAndResultRecordsAndNoDenial(t *testing.T) {
	h, _, cs := spentQuota(t)

	callToolError(t, cs, "get_shop", nil)

	recs := h.audit.records(t)
	if len(recs) != 6 {
		t.Fatalf("got %d audit records, want an attempt and a result for each of 3 calls:\n%s", len(recs), h.audit)
	}
	for _, rec := range recs {
		if rec["phase"] == "denied" {
			t.Errorf("denial record for a Quota refusal: %v", rec)
		}
	}
	attempt, result := recs[4], recs[5]
	checkFields(t, attempt, map[string]any{"phase": "attempt", "tool": "get_shop", "decision": "allow"})
	checkFields(t, result, map[string]any{"phase": "result", "tool": "get_shop", "decision": "allow", "outcome": "error"})
	// Distinct from the Store's rate limit, whose error names Shopify's
	// rate limit for the Store.
	if got, _ := result["error"].(string); !strings.Contains(got, "Quota") || strings.Contains(got, "Shopify's rate limit") {
		t.Errorf("result error = %q, want one naming the Caller's Quota, not Shopify's rate limit", got)
	}
	if shopify, ok := result["shopify"]; ok {
		t.Errorf("result record has Shopify fields, but Shopify wasn't called: %v", shopify)
	}
}
