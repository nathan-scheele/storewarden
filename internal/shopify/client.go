// Package shopify is a small GraphQL client for one Store's Admin API.
package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// APIVersion is the Admin API version every query runs against.
const APIVersion = "2026-10"

// maxResponseBytes caps how much of a Shopify response is read.
const maxResponseBytes = 10 << 20

// Client runs GraphQL queries against one Store. It gets its own access
// token with the client credentials grant and keeps it only in memory.
// Credentials and tokens never appear in its errors.
type Client struct {
	baseURL      string
	clientID     string
	clientSecret string
	hc           *http.Client

	mu     sync.Mutex // held while a token is fetched, so only one runs at a time
	token  string
	expiry time.Time
}

// New returns a Client for the Store at baseURL ("https://<name>.myshopify.com").
// If hc is nil, http.DefaultClient is used.
func New(baseURL, clientID, clientSecret string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		hc:           hc,
	}
}

// ThrottleStatus is the state of the Store's query cost bucket.
type ThrottleStatus struct {
	CurrentlyAvailable float64 `json:"currentlyAvailable"`
	MaximumAvailable   float64 `json:"maximumAvailable"`
	RestoreRate        float64 `json:"restoreRate"`
}

// Cost is what Shopify reports a query cost.
type Cost struct {
	RequestedQueryCost float64        `json:"requestedQueryCost"`
	ActualQueryCost    float64        `json:"actualQueryCost"`
	ThrottleStatus     ThrottleStatus `json:"throttleStatus"`
}

// Response is what Shopify reports about a query besides its data.
type Response struct {
	Cost      *Cost  // nil if Shopify didn't report it
	RequestID string // Shopify's X-Request-Id
}

type observerKey struct{}

// WithObserver returns a copy of ctx in which every Query that gets a
// response from Shopify passes it to observe, the way net/http/httptrace
// reports on requests. It lets the Audit record collect each call's costs
// and request IDs without every Tool passing them along.
func WithObserver(ctx context.Context, observe func(*Response)) context.Context {
	return context.WithValue(ctx, observerKey{}, observe)
}

// Query runs a GraphQL query and decodes its data into data.
func (c *Client) Query(ctx context.Context, query string, variables map[string]any, data any) (*Response, error) {
	resp, err := c.query(ctx, query, variables, data)
	if observe, ok := ctx.Value(observerKey{}).(func(*Response)); ok && resp != nil {
		observe(resp)
	}
	return resp, err
}

func (c *Client) query(ctx context.Context, query string, variables map[string]any, data any) (*Response, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/admin/api/"+APIVersion+"/graphql.json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Access-Token", token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shopify: graphql: %w", err)
	}
	defer resp.Body.Close()
	meta := &Response{RequestID: resp.Header.Get("X-Request-Id")}
	if resp.StatusCode != http.StatusOK {
		return meta, fmt.Errorf("shopify: graphql: HTTP %d", resp.StatusCode)
	}

	var out struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Extensions struct {
			Cost *Cost `json:"cost"`
		} `json:"extensions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return meta, fmt.Errorf("shopify: graphql: decoding response: %w", err)
	}
	meta.Cost = out.Extensions.Cost
	if len(out.Errors) > 0 {
		msgs := make([]string, len(out.Errors))
		for i, e := range out.Errors {
			msgs[i] = e.Message
		}
		return meta, fmt.Errorf("shopify: graphql: %s", strings.Join(msgs, "; "))
	}
	if err := json.Unmarshal(out.Data, data); err != nil {
		return meta, fmt.Errorf("shopify: graphql: decoding data: %w", err)
	}
	return meta, nil
}

// accessToken returns the cached token, fetching a new one with the client
// credentials grant once it has expired.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expiry) {
		return c.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/admin/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("shopify: access token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("shopify: access token: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"` // seconds
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("shopify: access token: decoding response: %w", err)
	}
	if out.AccessToken == "" {
		return "", errors.New("shopify: access token: response has no token")
	}
	c.token = out.AccessToken
	c.expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return c.token, nil
}
