package storageinventory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSourceErrorsNeverExposeResponseSecrets(t *testing.T) {
	t.Parallel()
	failure := errors.New("request https://operator:secret-password@pve/?token=secret-token failed; secret-body")
	for _, mode := range []string{"definitions", "all-nodes", "partial-node"} {
		t.Run(mode, func(t *testing.T) {
			source := sourceFor(t, "a")
			switch mode {
			case "definitions":
				source.definitionErr = failure
			case "all-nodes":
				source.errors["n1"], source.errors["n2"] = failure, failure
			case "partial-node":
				source.errors["n1"] = failure
			}
			c := collector(t, source, newClock())
			snapshot, err := c.Discover(context.Background(), policy("a"), Request{Nodes: []string{"n1", "n2"}})
			var output string
			if mode == "partial-node" {
				if err != nil {
					t.Fatal(err)
				}
				output = strings.Join(snapshot.Issues(), " ")
				if p, _ := snapshot.Pair("n1", "a"); p.Reason == "" {
					t.Fatal("failed node remained eligible")
				}
				if p, _ := snapshot.Pair("n2", "a"); p.Reason != "" {
					t.Fatal("healthy node lost eligibility")
				}
			} else {
				if err == nil {
					t.Fatal("failed observation succeeded")
				}
				output = err.Error()
				if mode == "definitions" && !errors.Is(err, failure) {
					t.Fatal("lost underlying cause")
				}
			}
			if !strings.Contains(output, "storage API request failed") {
				t.Fatalf("missing safe cause: %s", output)
			}
			for _, secret := range []string{"secret-password", "secret-token", "secret-body"} {
				if strings.Contains(output, secret) {
					t.Fatalf("API secret leaked in %s diagnostics", mode)
				}
			}
		})
	}
}
