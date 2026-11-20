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

func TestSanitizeToolName_DistinctBoundariesHashDivergence(t *testing.T) {
	// Two pairs where sanitized concatenated string is identical but boundaries differ
	srv1, tool1 := "long_enterprise_warehouse", "system_query_customer_transactions_detailed"
	srv2, tool2 := "long_enterprise", "warehouse_system_query_customer_transactions_detailed"

	name1 := SanitizeToolName(srv1, tool1)
	name2 := SanitizeToolName(srv2, tool2)

	if name1 == name2 {
		t.Fatalf("collision detected across boundary shift: %q == %q", name1, name2)
	}
}

func TestDisambiguateToolName_Divergence(t *testing.T) {
	// Short names that collide when sanitized
	srv1, tool1 := "a_b", "c"
	srv2, tool2 := "a", "b_c"

	d1 := DisambiguateToolName(srv1, tool1)
	d2 := DisambiguateToolName(srv2, tool2)

	if d1 == d2 {
		t.Fatalf("DisambiguateToolName collided: %q == %q", d1, d2)
	}
	if len(d1) > 64 || len(d2) > 64 {
		t.Fatalf("disambiguated names exceed 64 runes: len(d1)=%d, len(d2)=%d", len(d1), len(d2))
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
