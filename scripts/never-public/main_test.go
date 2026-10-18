package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree creates files under a temporary directory, from a map of
// slash-separated paths to contents, and returns the directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// mustFind runs the check on root and fails the test if it can't.
func mustFind(t *testing.T, root string) []string {
	t.Helper()
	problems, err := findProblems(root)
	if err != nil {
		t.Fatal(err)
	}
	return problems
}

// wantOneProblem fails the test unless there is exactly one problem and it
// names the file and contains want.
func wantOneProblem(t *testing.T, problems []string, file, want string) {
	t.Helper()
	if len(problems) != 1 {
		t.Fatalf("got %d problems, want 1: %q", len(problems), problems)
	}
	if !strings.HasPrefix(problems[0], file+":") || !strings.Contains(problems[0], want) {
		t.Errorf("got %q, want a problem in %s mentioning %q", problems[0], file, want)
	}
}

func TestFunnelListenerIsAProblem(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cmd/app/main.go": `package main

import (
	"tailscale.com/feature/buildfeatures"
	"tailscale.com/tsnet"
)

func main() {
	if buildfeatures.HasPeerAPIServer {
		return
	}
	ts := &tsnet.Server{}
	ts.ListenFunnel("tcp", ":443")
}
`,
	})
	wantOneProblem(t, mustFind(t, root), "cmd/app/main.go:13", "ListenFunnel")
}

func TestServeConfigChangeIsAProblem(t *testing.T) {
	root := writeTree(t, map[string]string{
		"internal/expose/expose.go": `package expose

import (
	"context"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
)

func Expose(ctx context.Context, lc *local.Client) error {
	return lc.SetServeConfig(ctx, &ipn.ServeConfig{})
}
`,
	})
	wantOneProblem(t, mustFind(t, root), "internal/expose/expose.go:11", "SetServeConfig")
}

func TestHostListenerIsAProblem(t *testing.T) {
	tests := []struct {
		name, imports, body, want string
	}{
		{"net.Listen", `"net"`, `net.Listen("tcp", ":8080")`, "net.Listen"},
		{"net.ListenPacket", `"net"`, `net.ListenPacket("udp", ":53")`, "net.ListenPacket"},
		{"net.ListenConfig", `"net"`, `var lc net.ListenConfig; _ = lc`, "net.ListenConfig"},
		{"renamed import", `stdnet "net"`, `stdnet.ListenTCP("tcp", nil)`, "net.ListenTCP"},
		{"http.ListenAndServe", `"net/http"`, `http.ListenAndServe(":8080", nil)`, "net/http.ListenAndServe"},
		{"Server.ListenAndServe", `"net/http"`, `srv := &http.Server{}; srv.ListenAndServe()`, "ListenAndServe"},
		{"Server.ListenAndServeTLS", `"net/http"`, `srv := &http.Server{}; srv.ListenAndServeTLS("", "")`, "ListenAndServeTLS"},
		{"tls.Listen", `"crypto/tls"`, `tls.Listen("tcp", ":443", nil)`, "crypto/tls.Listen"},
		{"httptest server", `"net/http/httptest"`, `httptest.NewServer(nil)`, "net/http/httptest.NewServer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"x.go": "package x\n\nimport " + tt.imports + "\n\nfunc f() {\n\t" + tt.body + "\n}\n",
			})
			wantOneProblem(t, mustFind(t, root), "x.go:6", tt.want)
		})
	}
}

func TestAllowedCodeIsNotAProblem(t *testing.T) {
	hostListener := "package x\n\nimport \"net\"\n\nfunc f() { net.Listen(\"tcp\", \":80\") }\n"
	root := writeTree(t, map[string]string{
		// The tsnet listener, and the forbidden names in comments and strings.
		"cmd/app/main.go": `package main

import (
	"fmt"

	"tailscale.com/feature/buildfeatures"
	"tailscale.com/tsnet"
)

// main never calls ListenFunnel or net.Listen.
func main() {
	if buildfeatures.HasPeerAPIServer {
		return
	}
	ts := &tsnet.Server{}
	ts.Listen("tcp", ":80")
	fmt.Println("no SetServeConfig here")
}
`,
		// Tests, test data and hidden directories (such as other worktrees)
		// are not part of the server.
		"cmd/app/main_test.go":         hostListener,
		"cmd/app/testdata/x.go":        hostListener,
		".claude/worktrees/other/x.go": hostListener,
		"internal/.hidden/x.go":        hostListener,
	})
	if problems := mustFind(t, root); len(problems) != 0 {
		t.Errorf("got problems in allowed code: %q", problems)
	}
}

// A plain go build brings back tsnet's peer API, which listens on every
// host interface, so a program using tsnet must refuse to run without
// -tags ts_omit_peerapiserver (ADR 0003).
func TestTsnetWithoutPeerAPICheckIsAProblem(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cmd/app/main.go": `package main

import "tailscale.com/tsnet"

func main() {
	ts := &tsnet.Server{}
	ts.Listen("tcp", ":80")
}
`,
		// The check in a test file doesn't count.
		"cmd/app/main_test.go": `package main

import "tailscale.com/feature/buildfeatures"

var _ = buildfeatures.HasPeerAPIServer
`,
	})
	wantOneProblem(t, mustFind(t, root), "cmd/app/main.go:3", "HasPeerAPIServer")
}

func TestPeerAPICheckInAnotherFileOfThePackageCounts(t *testing.T) {
	root := writeTree(t, map[string]string{
		"cmd/app/main.go": `package main

import "tailscale.com/tsnet"

func main() {
	ts := &tsnet.Server{}
	ts.Listen("tcp", ":80")
}
`,
		"cmd/app/build.go": `package main

import "tailscale.com/feature/buildfeatures"

func init() {
	if buildfeatures.HasPeerAPIServer {
		panic("build with -tags ts_omit_peerapiserver")
	}
}
`,
	})
	if problems := mustFind(t, root); len(problems) != 0 {
		t.Errorf("got problems: %q", problems)
	}
}
