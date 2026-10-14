package replaytools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestEngineeringHasNoIssueCatalogueCapability(t *testing.T) {
	// Even an old registration containing catalogue paths cannot grant this
	// capability to the engineering role.
	server := Server{Config: Config{IssueDirectory: "old-view", IssueContext: "old-handoff"}}
	for _, name := range RoutingNames {
		if _, err := server.Call(context.Background(), name, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "Routing Agent") {
			t.Fatal("engineering acquired catalogue access", name, err)
		}
	}
	var discovery bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n"), &discovery); err != nil {
		t.Fatal(err)
	}
	for _, name := range RoutingNames {
		if strings.Contains(discovery.String(), `"name":"`+name+`"`) {
			t.Fatal("engineering discovery exposed a routing tool", discovery.String())
		}
	}
}
