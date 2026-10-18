package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// An operator's typo in --quota-share stops storewarden before it opens the
// audit log, so before it joins the tailnet or calls Shopify.
func TestQuotaShareOutOfRangeRefusesToStart(t *testing.T) {
	t.Setenv("SHOPIFY_CLIENT_ID", testClientID)
	t.Setenv("SHOPIFY_CLIENT_SECRET", testClientSecret)
	// If run ever got past the Quota share, this audit log couldn't be
	// opened, so it would stop there with a different error instead of
	// starting the tailnet node.
	unopenable := filepath.Join(t.TempDir(), "missing", "audit.log")
	for _, share := range []string{"0", "-0.5", "1.01", "NaN"} {
		t.Run(share, func(t *testing.T) {
			err := run([]string{
				"--store", "storewarden-dev.myshopify.com",
				"--state-dir", t.TempDir(),
				"--audit-log", unopenable,
				"--quota-share", share,
			})
			if err == nil || !strings.Contains(err.Error(), "--quota-share") {
				t.Errorf("run with --quota-share %s = %v, want a --quota-share error", share, err)
			}
		})
	}
}
