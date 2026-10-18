// Package shopify is a small GraphQL client for one Store's Admin API.
package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// renewBefore is how long before its expiry an access token is replaced.
const renewBefore = 5 * time.Minute

// defaultTokenLifetime is how long an access token is taken to last when
// Shopify's response doesn't say. Shopify documents 24 hours.
const defaultTokenLifetime = 24 * time.Hour

// clock is the client's source of time. Tests replace it so they never
// really sleep.
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time // like time.After
}

// realClock is the clock from package time.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Client runs GraphQL queries against one Store. It gets its own access
// token with the client credentials grant and keeps it only in memory.
// Credentials and tokens never appear in its errors.
type Client struct {
	baseURL      string
	clientID     string
	clientSecret string
	hc           *http.Client
	logger       *slog.Logger
	clock        clock
	bucket       bucket // the Store's query cost bucket, as far as the client knows it

	mu     sync.Mutex // held while a token is fetched, so only one runs at a time
	token  string
	expiry time.Time
}

// New returns a Client for the Store at baseURL ("https://<name>.myshopify.com").
// If hc is nil, http.DefaultClient is used. Warnings go to logger.
func New(baseURL, clientID, clientSecret string, hc *http.Client, logger *slog.Logger) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		hc:           hc,
		logger:       logger,
		clock:        realClock{},
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

// Meta is what Shopify reports about a query besides its data.
type Meta struct {
	Cost      *Cost  // nil if Shopify didn't report it
	RequestID string // Shopify's X-Request-Id
}

type observerKey struct{}

// WithObserver returns a copy of ctx in which Query passes observe every
// response it gets from Shopify, the way net/http/httptrace reports on
// requests. A throttled query and its retry are two responses. It lets the
// Audit record collect each call's costs and request IDs without every Tool
// passing them along.
func WithObserver(ctx context.Context, observe func(*Meta)) context.Context {
	return context.WithValue(ctx, observerKey{}, observe)
}

// report passes resp to ctx's observer, if it has one and resp isn't nil.
func report(ctx context.Context, resp *Meta) {
	if observe, ok := ctx.Value(observerKey{}).(func(*Meta)); ok && resp != nil {
		observe(resp)
	}
}

// Query runs a GraphQL query and decodes its data into data. maxCost is the
// most the query can cost, in Shopify's query cost points.
//
// It first waits for the Store's cost bucket to hold maxCost, or returns a
// *ThrottledError at once if that wait would outlast ctx's deadline. If
// Shopify throttles the query anyway, it is retried once the same way.
// Shopify's errors come back as GraphQLErrors or UserErrors. The Meta,
// when not nil, is from the last request Shopify answered, even if there is
// an error.
func (c *Client) Query(ctx context.Context, query string, maxCost float64, variables map[string]any, data any) (*Meta, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, err
	}
	resp, err := c.queryOnce(ctx, maxCost, body, data)
	report(ctx, resp)
	if !throttled(err) {
		return resp, err
	}
	// The bucket now holds Shopify's figures from the throttled response,
	// so the retry waits just long enough.
	retryResp, err := c.queryOnce(ctx, maxCost, body, data)
	report(ctx, retryResp)
	if retryResp != nil {
		resp = retryResp
	}
	if throttled(err) {
		return resp, &ThrottledError{Wait: c.bucket.wait(c.clock.Now(), maxCost)}
	}
	return resp, err
}

// throttled reports whether err is Shopify's THROTTLED error.
func throttled(err error) bool {
	var errs GraphQLErrors
	return errors.As(err, &errs) && errs.has("THROTTLED")
}

// queryOnce waits for capacity, then sends one query and decodes the answer.
func (c *Client) queryOnce(ctx context.Context, maxCost float64, body []byte, data any) (*Meta, error) {
	if err := c.waitForCapacity(ctx, maxCost); err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	meta := &Meta{RequestID: resp.Header.Get("X-Request-Id")}
	if resp.StatusCode != http.StatusOK {
		return meta, fmt.Errorf("shopify: graphql: HTTP %d", resp.StatusCode)
	}

	var out struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
		Extensions struct {
			Cost *Cost `json:"cost"`
		} `json:"extensions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return meta, fmt.Errorf("shopify: graphql: decoding response: %w", err)
	}
	meta.Cost = out.Extensions.Cost
	c.bucket.record(c.clock.Now(), meta.Cost)
	if len(out.Errors) > 0 {
		errs := make(GraphQLErrors, len(out.Errors))
		for i, e := range out.Errors {
			errs[i] = GraphQLError{Message: e.Message, Code: e.Extensions.Code}
		}
		return meta, errs
	}
	if err := json.Unmarshal(out.Data, data); err != nil {
		return meta, fmt.Errorf("shopify: graphql: decoding data: %w", err)
	}
	if userErrs := findUserErrors(out.Data); len(userErrs) > 0 {
		return meta, userErrs
	}
	return meta, nil
}

// post sends a GraphQL request body with the access token. If Shopify
// refuses the token (HTTP 401), it gets a new token and sends the body once
// more. The caller closes the response body.
func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	token, err := c.accessToken(ctx, "")
	if err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, token, body)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	resp.Body.Close()
	token, err = c.accessToken(ctx, token)
	if err != nil {
		return nil, err
	}
	return c.send(ctx, token, body)
}

// send makes one GraphQL request.
func (c *Client) send(ctx context.Context, token string, body []byte) (*http.Response, error) {
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
	return resp, nil
}

// accessToken returns the cached token, fetching a new one with the client
// credentials grant once it is within renewBefore of its expiry. refused is
// a token Shopify has just refused, or "": if it is still the cached token,
// it is replaced. Callers that were refused the same token at the same time
// therefore cause only one new token between them.
func (c *Client) accessToken(ctx context.Context, refused string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.token != refused && c.clock.Now().Before(c.expiry.Add(-renewBefore)) {
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
	lifetime := time.Duration(out.ExpiresIn) * time.Second
	if lifetime <= 0 {
		// Without this, a token that seems to expire at once would be
		// replaced before every query.
		c.logger.Warn("Shopify access token has no expires_in; assuming it lasts the documented 24 hours",
			"expires_in", out.ExpiresIn)
		lifetime = defaultTokenLifetime
	}
	c.token = out.AccessToken
	c.expiry = c.clock.Now().Add(lifetime)
	return c.token, nil
}
