package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPlanChangeToolPositive(t *testing.T) {
	sess := newGraphTestServer(t, seedMixedConfidence)
	ctx := context.Background()

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{
		Name: "plan_change",
		Arguments: map[string]any{
			"symbol": "auth.Login",
		},
	})
	if err != nil {
		t.Fatalf("call plan_change: %v", err)
	}

	text := resultText(res)
	if !strings.Contains(text, "Change plan for") {
		t.Errorf("expected summary to contain 'Change plan for', got:\n%s", text)
	}
	if !strings.Contains(text, "Step 1:") {
		t.Errorf("expected plan to contain 'Step 1:', got:\n%s", text)
	}
}

func TestPlanChangeToolNegativeEmpty(t *testing.T) {
	sess := newGraphTestServer(t, nil)
	ctx := context.Background()

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{
		Name: "plan_change",
		Arguments: map[string]any{
			"symbol": "",
		},
	})
	if err != nil {
		t.Fatalf("unexpected call error: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected isError=true for empty symbol")
	}
}

func TestPlanChangeToolNegativeNotFound(t *testing.T) {
	sess := newGraphTestServer(t, nil)
	ctx := context.Background()

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{
		Name: "plan_change",
		Arguments: map[string]any{
			"symbol": "NoSuchService",
		},
	})
	if err != nil {
		t.Fatalf("unexpected call error: %v", err)
	}
	text := resultText(res)
	if !strings.Contains(text, "No symbol named") && !strings.Contains(text, "not found") && !strings.Contains(text, "NoSuchService") {
		t.Errorf("expected guidance for missing symbol, got:\n%s", text)
	}
}

func TestDetectBlindspotsToolPositive(t *testing.T) {
	sess := newGraphTestServer(t, nil)
	ctx := context.Background()

	res, err := sess.CallTool(ctx, &sdk.CallToolParams{
		Name:      "detect_blindspots",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call detect_blindspots: %v", err)
	}

	text := resultText(res)
	if !strings.Contains(text, "blindspots detected") {
		t.Errorf("expected output to mention blindspots, got:\n%s", text)
	}
}
