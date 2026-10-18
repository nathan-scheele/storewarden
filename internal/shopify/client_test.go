package shopify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

const (
	testClientID     = "test-client-id"
	testClientSecret = "test-client-secret"
)

// fakeClock is a clock that only moves when told to. After moves it forward
// by the full wait at once, so a test never really sleeps.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func newFakeClock() *fakeClock {
	// Start at the real time so context deadlines, which use the real clock,
	// line up with the fake one.
	return &fakeClock{now: time.Now()}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.waits = append(f.waits, d)
	ch := make(chan time.Time, 1)
	ch <- f.now
	return ch
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Waits returns every wait the client asked for.
func (f *fakeClock) Waits() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.waits...)
}

// fakeShopify is an httptest Shopify. It issues numbered tokens
// ("token-1", "token-2", ...) that last 24 hours, and answers GraphQL
// requests with graphql, which sees the token each request carried.
type fakeShopify struct {
	*httptest.Server

	mu      sync.Mutex
	issued  int
	tokens  []string // token sent with each GraphQL request, in order
	graphql func(w http.ResponseWriter, token string, call int)
}

func newFakeShopify(t *testing.T, graphql func(w http.ResponseWriter, token string, call int)) *fakeShopify {
	t.Helper()
	f := &fakeShopify{graphql: graphql}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "client_credentials" ||
			r.FormValue("client_id") != testClientID || r.FormValue("client_secret") != testClientSecret {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.issued++
		token := fmt.Sprintf("token-%d", f.issued)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": token, "expires_in": 86399})
	})
	mux.HandleFunc("POST /admin/api/"+APIVersion+"/graphql.json", func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Shopify-Access-Token")
		f.mu.Lock()
		f.tokens = append(f.tokens, token)
		call := len(f.tokens)
		graphql := f.graphql
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", fmt.Sprintf("request-%d", call))
		graphql(w, token, call)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// Issued returns how many tokens Shopify has issued.
func (f *fakeShopify) Issued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issued
}

// Tokens returns the token each GraphQL request carried.
func (f *fakeShopify) Tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

// shopBody is a successful response with a full cost bucket.
const shopBody = `{
  "data": {"shop": {"name": "storewarden-dev"}},
  "extensions": {"cost": {"requestedQueryCost": 2, "actualQueryCost": 2,
    "throttleStatus": {"maximumAvailable": 2000.0, "currentlyAvailable": 1998, "restoreRate": 100.0}}}
}`

func answer(body string) func(http.ResponseWriter, string, int) {
	return func(w http.ResponseWriter, _ string, _ int) { fmt.Fprint(w, body) }
}

func newTestClient(f *fakeShopify, clock *fakeClock) *Client {
	c := New(f.URL, testClientID, testClientSecret, nil)
	c.clock = clock
	return c
}

type shopData struct {
	Shop struct {
		Name string `json:"name"`
	} `json:"shop"`
}

func query(t *testing.T, c *Client) (*Response, error) {
	t.Helper()
	var data shopData
	return c.Query(t.Context(), "query { shop { name } }", nil, &data)
}

func mustQuery(t *testing.T, c *Client) *Response {
	t.Helper()
	resp, err := query(t, c)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestTokenIsIssuedOnceAndReused(t *testing.T) {
	f := newFakeShopify(t, answer(shopBody))
	c := newTestClient(f, newFakeClock())

	mustQuery(t, c)
	mustQuery(t, c)

	if got := f.Issued(); got != 1 {
		t.Errorf("tokens issued = %d, want 1", got)
	}
	if got := f.Tokens(); len(got) != 2 || got[0] != "token-1" || got[1] != "token-1" {
		t.Errorf("tokens sent = %v, want [token-1 token-1]", got)
	}
}

func TestTokenIsReissuedFiveMinutesBeforeExpiry(t *testing.T) {
	f := newFakeShopify(t, answer(shopBody))
	clock := newFakeClock()
	c := newTestClient(f, clock)

	mustQuery(t, c)
	// The token lasts 86399s; 5m01s before then it is still used.
	clock.Advance(86399*time.Second - 5*time.Minute - time.Second)
	mustQuery(t, c)
	// Within five minutes of expiry it is replaced.
	clock.Advance(2 * time.Second)
	mustQuery(t, c)

	want := []string{"token-1", "token-1", "token-2"}
	if got := f.Tokens(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tokens sent = %v, want %v", got, want)
	}
}

// refusing answers 401 to any request carrying one of the refused tokens.
func refusing(refused ...string) func(http.ResponseWriter, string, int) {
	return func(w http.ResponseWriter, token string, _ int) {
		for _, r := range refused {
			if token == r {
				http.Error(w, `{"errors":"[API] Invalid API key or access token"}`, http.StatusUnauthorized)
				return
			}
		}
		fmt.Fprint(w, shopBody)
	}
}

func TestTokenIsReissuedOnceAfter401AndTheRequestRetried(t *testing.T) {
	f := newFakeShopify(t, refusing("token-1"))
	c := newTestClient(f, newFakeClock())

	var data shopData
	if _, err := c.Query(t.Context(), "query { shop { name } }", nil, &data); err != nil {
		t.Fatal(err)
	}
	if data.Shop.Name != "storewarden-dev" {
		t.Errorf("shop name = %q, want storewarden-dev", data.Shop.Name)
	}
	want := []string{"token-1", "token-2"}
	if got := f.Tokens(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tokens sent = %v, want %v", got, want)
	}
}

func TestA401AfterReissueIsAnError(t *testing.T) {
	f := newFakeShopify(t, refusing("token-1", "token-2", "token-3"))
	c := newTestClient(f, newFakeClock())

	if _, err := query(t, c); err == nil {
		t.Fatal("query succeeded, want an error")
	}
	if got := f.Issued(); got != 2 {
		t.Errorf("tokens issued = %d, want 2 (the first and one reissue)", got)
	}
	if got := len(f.Tokens()); got != 2 {
		t.Errorf("GraphQL requests = %d, want 2 (the first and one retry)", got)
	}
}

// concurrently runs n queries at once and fails the test if any fails.
func concurrently(t *testing.T, c *Client, n int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			var data shopData
			_, err := c.Query(t.Context(), "query { shop { name } }", nil, &data)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestConcurrentCallersShareOneTokenRequest(t *testing.T) {
	f := newFakeShopify(t, answer(shopBody))
	c := newTestClient(f, newFakeClock())

	concurrently(t, c, 20)

	if got := f.Issued(); got != 1 {
		t.Errorf("tokens issued = %d, want 1", got)
	}
}

func TestGraphQLErrorsAreTyped(t *testing.T) {
	f := newFakeShopify(t, answer(`{"errors": [
	  {"message": "Field 'nope' doesn't exist on type 'Shop'", "extensions": {"code": "undefinedField"}},
	  {"message": "Something else"}
	]}`))
	c := newTestClient(f, newFakeClock())

	_, err := query(t, c)
	var got GraphQLErrors
	if !errors.As(err, &got) {
		t.Fatalf("error = %v (%T), want GraphQLErrors", err, err)
	}
	want := GraphQLErrors{
		{Message: "Field 'nope' doesn't exist on type 'Shop'", Code: "undefinedField"},
		{Message: "Something else"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("errors = %+v, want %+v", got, want)
	}
}

func TestUserErrorsAreTypedAndTheDataStillDecoded(t *testing.T) {
	f := newFakeShopify(t, answer(`{"data": {"inventorySetQuantities": {
	  "inventoryAdjustmentGroup": null,
	  "userErrors": [{"field": ["input", "quantities", "0"], "message": "Quantity is stale", "code": "TEST_CODE"}]
	}}}`))
	c := newTestClient(f, newFakeClock())

	var data struct {
		InventorySetQuantities struct {
			UserErrors []UserError `json:"userErrors"`
		} `json:"inventorySetQuantities"`
	}
	_, err := c.Query(t.Context(), "mutation { inventorySetQuantities { userErrors { field message code } } }", nil, &data)
	var got UserErrors
	if !errors.As(err, &got) {
		t.Fatalf("error = %v (%T), want UserErrors", err, err)
	}
	want := UserErrors{{Field: []string{"input", "quantities", "0"}, Message: "Quantity is stale", Code: "TEST_CODE"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("user errors = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(data.InventorySetQuantities.UserErrors, []UserError(want)) {
		t.Errorf("decoded data = %+v, want the user errors too", data)
	}
}

func TestEmptyUserErrorsAreNotAnError(t *testing.T) {
	f := newFakeShopify(t, answer(`{"data": {
	  "inventorySetQuantities": {"userErrors": []},
	  "nodes": [{"id": "gid://shopify/Product/1"}],
	  "shop": null
	}}`))
	c := newTestClient(f, newFakeClock())

	var data map[string]any
	if _, err := c.Query(t.Context(), "mutation { x }", nil, &data); err != nil {
		t.Fatal(err)
	}
}

func TestRequestIDIsReturned(t *testing.T) {
	f := newFakeShopify(t, func(w http.ResponseWriter, _ string, call int) {
		if call == 1 {
			fmt.Fprint(w, shopBody)
			return
		}
		fmt.Fprint(w, `{"errors": [{"message": "Internal error"}]}`)
	})
	c := newTestClient(f, newFakeClock())

	if resp := mustQuery(t, c); resp.RequestID != "request-1" {
		t.Errorf("request ID = %q, want request-1", resp.RequestID)
	}
	resp, err := query(t, c)
	if err == nil {
		t.Fatal("query succeeded, want an error")
	}
	if resp == nil || resp.RequestID != "request-2" {
		t.Errorf("response with an error = %+v, want request ID request-2", resp)
	}
}

// costBody is a successful response for a query whose requested cost is
// 100, leaving the bucket with available of its 1000 points, refilling at
// 50 a second.
func costBody(available float64) string {
	return fmt.Sprintf(`{
	  "data": {"shop": {"name": "storewarden-dev"}},
	  "extensions": {"cost": {"requestedQueryCost": 100, "actualQueryCost": 100,
	    "throttleStatus": {"maximumAvailable": 1000.0, "currentlyAvailable": %g, "restoreRate": 50.0}}}
	}`, available)
}

// throttledBody is a THROTTLED response for a query whose requested cost is
// 100, with the bucket holding available points.
//
// Assumed shape: no throttled response has been recorded from the dev store
// yet (spec #1, Further Notes). This follows Shopify's rate limit docs: HTTP
// 200, an error with extensions.code "THROTTLED", and the usual cost report
// with a null actual cost. Replace it with a recording once one exists.
func throttledBody(available float64) string {
	return fmt.Sprintf(`{
	  "errors": [{"message": "Throttled", "extensions": {"code": "THROTTLED",
	    "documentation": "https://shopify.dev/api/usage/rate-limits"}}],
	  "extensions": {"cost": {"requestedQueryCost": 100, "actualQueryCost": null,
	    "throttleStatus": {"maximumAvailable": 1000.0, "currentlyAvailable": %g, "restoreRate": 50.0}}}
	}`, available)
}

// answers answers the nth GraphQL request with bodies[n-1], and repeats the
// last body after that.
func answers(bodies ...string) func(http.ResponseWriter, string, int) {
	return func(w http.ResponseWriter, _ string, call int) {
		fmt.Fprint(w, bodies[min(call, len(bodies))-1])
	}
}

func queryBy(t *testing.T, c *Client, deadline time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	var data shopData
	_, err := c.Query(ctx, "query { shop { name } }", nil, &data)
	return err
}

func TestBucketIsAssumedFullUntilTheFirstResponse(t *testing.T) {
	f := newFakeShopify(t, answer(costBody(1000)))
	clock := newFakeClock()
	c := newTestClient(f, clock)

	mustQuery(t, c)

	if got := clock.Waits(); len(got) != 0 {
		t.Errorf("waits = %v, want none", got)
	}
}

func TestWaitsWhenCapacityRefillsBeforeTheDeadline(t *testing.T) {
	// The first response leaves 25 points; the query costs 100 and the
	// bucket refills at 50 a second, so the next call needs 1.5s.
	f := newFakeShopify(t, answer(costBody(25)))
	clock := newFakeClock()
	c := newTestClient(f, clock)
	mustQuery(t, c)

	if err := queryBy(t, c, time.Hour); err != nil {
		t.Fatal(err)
	}

	if got, want := clock.Waits(), []time.Duration{1500 * time.Millisecond}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
	if got := len(f.Tokens()); got != 2 {
		t.Errorf("GraphQL requests = %d, want 2", got)
	}
}

func TestBucketRefillsWithTime(t *testing.T) {
	f := newFakeShopify(t, answer(costBody(25)))
	clock := newFakeClock()
	c := newTestClient(f, clock)
	mustQuery(t, c)

	clock.Advance(time.Second) // 25 + 50 = 75 points: 0.5s still to go
	mustQuery(t, c)

	if got, want := clock.Waits(), []time.Duration{500 * time.Millisecond}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
}

func TestBucketUpdatesFromEveryResponse(t *testing.T) {
	// The second response reports a full bucket, so the third call doesn't wait.
	f := newFakeShopify(t, answers(costBody(25), costBody(1000)))
	clock := newFakeClock()
	c := newTestClient(f, clock)

	mustQuery(t, c)
	mustQuery(t, c) // waits 1.5s
	mustQuery(t, c)

	if got, want := clock.Waits(), []time.Duration{1500 * time.Millisecond}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
}

func TestFailsFastWhenCapacityWontRefillBeforeTheDeadline(t *testing.T) {
	f := newFakeShopify(t, answer(costBody(25)))
	clock := newFakeClock()
	c := newTestClient(f, clock)
	mustQuery(t, c)

	err := queryBy(t, c, time.Second)

	var throttled *ThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("error = %v (%T), want *ThrottledError", err, err)
	}
	if throttled.Wait != 1500*time.Millisecond {
		t.Errorf("suggested wait = %v, want 1.5s", throttled.Wait)
	}
	if got := clock.Waits(); len(got) != 0 {
		t.Errorf("waits = %v, want none", got)
	}
	if got := len(f.Tokens()); got != 1 {
		t.Errorf("GraphQL requests = %d, want 1 (the throttled call never reached Shopify)", got)
	}
}

func TestAThrottledResponseIsRetriedOnceAfterTheWait(t *testing.T) {
	f := newFakeShopify(t, answers(throttledBody(25), costBody(0)))
	clock := newFakeClock()
	c := newTestClient(f, clock)

	var data shopData
	resp, err := c.Query(t.Context(), "query { shop { name } }", nil, &data)
	if err != nil {
		t.Fatal(err)
	}

	if data.Shop.Name != "storewarden-dev" || resp.RequestID != "request-2" {
		t.Errorf("got %q from %q, want the retry's result", data.Shop.Name, resp.RequestID)
	}
	if got, want := clock.Waits(), []time.Duration{1500 * time.Millisecond}; !reflect.DeepEqual(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
	if got := len(f.Tokens()); got != 2 {
		t.Errorf("GraphQL requests = %d, want 2", got)
	}
}

func TestAThrottledRetryIsNotRetriedAgain(t *testing.T) {
	f := newFakeShopify(t, answer(throttledBody(25)))
	c := newTestClient(f, newFakeClock())

	resp, err := query(t, c)

	var throttled *ThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("error = %v (%T), want *ThrottledError", err, err)
	}
	if throttled.Wait != 1500*time.Millisecond {
		t.Errorf("suggested wait = %v, want 1.5s", throttled.Wait)
	}
	if resp == nil || resp.RequestID != "request-2" {
		t.Errorf("response = %+v, want the retry's request ID", resp)
	}
	if got := len(f.Tokens()); got != 2 {
		t.Errorf("GraphQL requests = %d, want 2", got)
	}
}

func TestAThrottledResponseFailsFastWhenTheRetryWouldMissTheDeadline(t *testing.T) {
	f := newFakeShopify(t, answer(throttledBody(25)))
	c := newTestClient(f, newFakeClock())

	err := queryBy(t, c, time.Second)

	var throttled *ThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("error = %v (%T), want *ThrottledError", err, err)
	}
	if got := len(f.Tokens()); got != 1 {
		t.Errorf("GraphQL requests = %d, want 1", got)
	}
}

func TestConcurrentCallersRefusedTogetherShareOneReissue(t *testing.T) {
	f := newFakeShopify(t, answer(shopBody))
	c := newTestClient(f, newFakeClock())
	mustQuery(t, c) // token-1 is now cached
	f.mu.Lock()
	f.graphql = refusing("token-1")
	f.mu.Unlock()

	concurrently(t, c, 20)

	if got := f.Issued(); got != 2 {
		t.Errorf("tokens issued = %d, want 2 (the first and one reissue)", got)
	}
}
