package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/shared"
)

func TestSanitizeToolName_Standard(t *testing.T) {
	tests := []struct {
		server   string
		tool     string
		expected string
	}{
		{"sqlite", "read_query", "mcp_sqlite_read_query"},
		{"postgres-db", "list_tables", "mcp_postgres-db_list_tables"},
		{"api/v1", "fetch.data", "mcp_api_v1_fetch_data"},
		{"", "run", "mcp_tool_run"},
	}

	for _, tc := range tests {
		got := SanitizeToolName(tc.server, tc.tool)
		if got != tc.expected {
			t.Errorf("SanitizeToolName(%q, %q) = %q, expected %q", tc.server, tc.tool, got, tc.expected)
		}
	}
}

func TestSanitizeToolName_CollisionResistanceOver64Runes(t *testing.T) {
	server := "enterprise_data_warehouse_analytics_system"
	tool1 := "query_customer_financial_transactions_by_account_id_detailed"
	tool2 := "query_customer_financial_transactions_by_timestamp_detailed"

	name1 := SanitizeToolName(server, tool1)
	name2 := SanitizeToolName(server, tool2)

	if len(name1) != 64 {
		t.Errorf("expected len(name1) to be exactly 64, got %d (%s)", len(name1), name1)
	}
	if len(name2) != 64 {
		t.Errorf("expected len(name2) to be exactly 64, got %d (%s)", len(name2), name2)
	}

	// Verify collision resistance: the two long names must not collapse to identical strings
	if name1 == name2 {
		t.Fatalf("collision detected! Both long tool names produced %q", name1)
	}
}

func TestToolAdapter_ParametersCanonicalization(t *testing.T) {
	mgr := NewClientManager(nil)
	rawSchema := map[string]any{
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
	}
	adapter := NewToolAdapter("test", mcp.Tool{
		Name:        "query",
		Description: "Run query",
		InputSchema: rawSchema,
	}, mgr, 5*time.Second)

	params := adapter.Parameters()
	fnParams, ok := params.(shared.FunctionParameters)
	if !ok {
		t.Fatalf("expected shared.FunctionParameters, got %T", params)
	}
	if fnParams["type"] != "object" {
		t.Errorf("expected type 'object', got %v", fnParams["type"])
	}
}

func TestToolAdapter_InvalidJSONArguments(t *testing.T) {
	mgr := NewClientManager(nil)
	adapter := NewToolAdapter("test", mcp.Tool{
		Name: "query",
	}, mgr, 5*time.Second)

	out, err := adapter.Execute(context.Background(), json.RawMessage(`{not a valid json`))
	if err != nil {
		t.Fatalf("expected nil error for tool parameter syntax error, got: %v", err)
	}
	if !strings.Contains(out, "MCP tool parameter error") {
		t.Errorf("expected parameter error message, got %q", out)
	}
}

func TestToolAdapter_DisconnectedServerError(t *testing.T) {
	mgr := NewClientManager(nil)
	adapter := NewToolAdapter("missing-server", mcp.Tool{
		Name: "query",
	}, mgr, 5*time.Second)

	out, err := adapter.Execute(context.Background(), json.RawMessage(`{"query": "SELECT 1"}`))
	if err != nil {
		t.Fatalf("expected nil error (formatted string return), got: %v", err)
	}
	if !strings.Contains(out, "MCP tool error") {
		t.Errorf("expected MCP tool error, got: %s", out)
	}
}
