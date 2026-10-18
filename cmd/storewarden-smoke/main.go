// Command storewarden-smoke is the test Caller for storewarden's manual
// smoke test (scripts/smoke-tailnet.sh). It joins the tailnet as its own
// ephemeral, tagged node, prints the tailnet's name as one JSON line, then
// reads commands from stdin, one per line, and answers each with one JSON
// line. "check" calls storewarden once, as an MCP client. "tools",
// "move ITEM LOCATION", "read-only", "quota" and "quota-other" run Part 2 of
// the smoke test (tools.go, move.go, quota.go). It stops at the end of stdin
// and removes its node from the tailnet.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tailscale.com/feature/buildfeatures"
	"tailscale.com/tsnet"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var (
		target   = flag.String("target", "", "storewarden's tailnet `address` (required)")
		hostname = flag.String("hostname", "storewarden-smoke", "tailnet node `name`")
		tags     = flag.String("tags", "tag:storewarden-smoke", "comma-separated `tags` the node advertises")
	)
	flag.Parse()
	if *target == "" {
		return errors.New("--target is required")
	}
	// Built in, tsnet's peer API listens on every host interface
	// (docs/adr/0003-build-without-the-peer-api-server.md).
	if buildfeatures.HasPeerAPIServer {
		return errors.New("storewarden-smoke must be built with -tags ts_omit_peerapiserver")
	}

	dir, err := os.MkdirTemp("", "storewarden-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	// tsnet reads TS_AUTHKEY from the environment. The node is ephemeral and
	// its state is thrown away, so every run is a fresh node.
	ts := &tsnet.Server{
		Hostname:      *hostname,
		Dir:           dir,
		Ephemeral:     true,
		AdvertiseTags: strings.Split(*tags, ","),
		UserLogf:      log.Printf,
	}
	if err := ts.Start(); err != nil {
		return err
	}
	defer ts.Close()

	ctx := context.Background()
	status, err := ts.Up(ctx)
	if err != nil {
		return fmt.Errorf("joining tailnet: %w", err)
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return err
	}
	// Remove the node from the tailnet now, rather than when the control
	// server notices it has gone.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := lc.Logout(ctx); err != nil {
			log.Printf("logging out: %v", err)
		}
	}()

	out := json.NewEncoder(os.Stdout)
	tailnet := ""
	if status.CurrentTailnet != nil {
		tailnet = status.CurrentTailnet.Name
	}
	if err := out.Encode(map[string]string{"tailnet": tailnet}); err != nil {
		return err
	}

	hc := ts.HTTPClient()
	base := "http://" + *target
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		// "quota" and "quota-other" take an inventory item ID after a space,
		// and "move" an inventory item ID and a location ID.
		command, item, _ := strings.Cut(in.Text(), " ")
		switch command {
		case "check":
			err = out.Encode(check(ctx, hc, base))
		case "tools":
			err = out.Encode(withSession(ctx, hc, base, runTools))
		case "read-only":
			err = out.Encode(withSession(ctx, hc, base, runReadOnly))
		case "move":
			item, location, _ := strings.Cut(item, " ")
			err = out.Encode(withSession(ctx, hc, base, func(ctx context.Context, s session) scenario {
				return runMove(ctx, s, item, location)
			}))
		case "quota":
			err = out.Encode(withSession(ctx, hc, base, func(ctx context.Context, s session) scenario {
				return runQuota(ctx, s, item)
			}))
		case "quota-other":
			err = out.Encode(withSession(ctx, hc, base, func(ctx context.Context, s session) scenario {
				return runQuotaOther(ctx, s, item)
			}))
		default:
			err = out.Encode(map[string]string{"error": "unknown command " + in.Text()})
		}
		if err != nil {
			return err
		}
	}
	return in.Err()
}

// result is what one check saw.
type result struct {
	Healthz     int            `json:"healthz"`      // HTTP status of GET /healthz
	HealthzBody string         `json:"healthz_body"` // its body, trimmed
	MCP         int            `json:"mcp"`          // HTTP status of a bare MCP initialize
	Tools       []string       `json:"tools"`        // tools/list
	GetShop     map[string]any `json:"get_shop,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// check calls storewarden the way a Caller would: health, then MCP
// tools/list and get_shop with the official SDK client.
func check(ctx context.Context, hc *http.Client, base string) result {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r := result{Tools: []string{}}

	health, err := get(ctx, hc, base+"/healthz")
	if err != nil {
		r.Error = "healthz: " + err.Error()
		return r
	}
	r.Healthz, r.HealthzBody = health.status, strings.TrimSpace(health.body)

	// A bare request shows the HTTP status, which the SDK client hides.
	status, err := postInitialize(ctx, hc, base+"/mcp")
	if err != nil {
		r.Error = "mcp: " + err.Error()
		return r
	}
	r.MCP = status
	if status != http.StatusOK {
		return r
	}

	cs, err := connect(ctx, hc, base)
	if err != nil {
		r.Error = "connect: " + err.Error()
		return r
	}
	defer cs.Close()

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		r.Error = "tools/list: " + err.Error()
		return r
	}
	for _, t := range list.Tools {
		r.Tools = append(r.Tools, t.Name)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_shop"})
	switch {
	case err != nil:
		r.Error = "get_shop: " + err.Error()
	case res.IsError:
		r.Error = "get_shop returned a tool error"
	default:
		r.GetShop, _ = res.StructuredContent.(map[string]any)
	}
	return r
}

// connect opens an MCP session with storewarden, over the tailnet.
func connect(ctx context.Context, hc *http.Client, base string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "storewarden-smoke", Version: "dev"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc}, nil)
}

// withSession runs a Part 2 scenario over a new MCP session.
func withSession(ctx context.Context, hc *http.Client, base string, run func(context.Context, session) scenario) scenario {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cs, err := connect(ctx, hc, base)
	if err != nil {
		return scenario{Steps: []step{}, Error: "connect: " + err.Error()}
	}
	defer cs.Close()
	return run(ctx, session{cs})
}

type response struct {
	status int
	body   string
}

func get(ctx context.Context, hc *http.Client, url string) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return response{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return response{resp.StatusCode, string(body)}, err
}

func postInitialize(ctx context.Context, hc *http.Client, url string) (int, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"storewarden-smoke","version":"dev"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
