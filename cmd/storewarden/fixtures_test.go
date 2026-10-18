package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every fixture is a real dev-store response, recorded by
// scripts/record-fixture.sh next to the query that produced it.
func TestEveryFixtureHasItsQuery(t *testing.T) {
	fixtures, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		if strings.HasSuffix(f, ".variables.json") {
			continue
		}
		query := strings.TrimSuffix(f, ".json") + ".graphql"
		if _, err := os.Stat(query); err != nil {
			t.Errorf("%s has no %s; record fixtures with scripts/record-fixture.sh", f, query)
		}
	}
}
