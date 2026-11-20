package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestResourceListerTool_Execution(t *testing.T) {
	mgr := NewClientManager(nil)
	lister := NewResourceListerTool(mgr)

	if lister.Name() != "mcp_list_resources" {
		t.Errorf("expected name mcp_list_resources, got %s", lister.Name())
	}

	ctx := context.Background()

	// Missing server argument
	out, err := lister.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !strings.Contains(out, "server argument is required") {
		t.Errorf("expected missing server error, got %s", out)
	}

	// Unknown / disconnected server
	out, err = lister.Execute(ctx, json.RawMessage(`{"server": "postgres"}`))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !strings.Contains(out, "not running or not found") {
		t.Errorf("expected server not running error, got %s", out)
	}
}

func TestResourceReaderTool_Execution(t *testing.T) {
	mgr := NewClientManager(nil)
	reader := NewResourceReaderTool(mgr)

	if reader.Name() != "mcp_read_resource" {
		t.Errorf("expected name mcp_read_resource, got %s", reader.Name())
	}

	ctx := context.Background()

	// Missing arguments
	out, err := reader.Execute(ctx, json.RawMessage(`{"server": "postgres"}`))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !strings.Contains(out, "server and uri arguments are required") {
		t.Errorf("expected missing arguments error, got %s", out)
	}

	// Unknown / disconnected server
	out, err = reader.Execute(ctx, json.RawMessage(`{"server": "postgres", "uri": "postgres://schema/public"}`))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !strings.Contains(out, "not running or not found") {
		t.Errorf("expected server not running error, got %s", out)
	}
}
