package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertUserMsgMultipleToolResults(t *testing.T) {
	mm := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "a", "content": "ra"},
		map[string]any{"type": "tool_result", "tool_use_id": "b", "content": []any{map[string]any{"type": "text", "text": "rb"}}, "is_error": true},
		map[string]any{"type": "text", "text": "继续"},
	}}
	out := convertUserMsg(mm)
	if len(out) != 3 {
		t.Fatalf("want 3 messages, got %d: %v", len(out), out)
	}
	m0, m1, m2 := out[0].(map[string]any), out[1].(map[string]any), out[2].(map[string]any)
	if m0["role"] != "tool" || m0["tool_call_id"] != "a" || m0["content"] != "ra" {
		t.Fatalf("bad first tool msg: %v", m0)
	}
	if m1["tool_call_id"] != "b" || !strings.Contains(m1["content"].(string), "rb") {
		t.Fatalf("bad second tool msg: %v", m1)
	}
	if m2["role"] != "user" {
		t.Fatalf("trailing user content lost: %v", m2)
	}
}

func TestAnthropicToolChoiceAndThinking(t *testing.T) {
	body := map[string]any{
		"model": "m", "max_tokens": float64(10), "temperature": 0.3,
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":       []any{map[string]any{"name": "f"}},
		"tool_choice": map[string]any{"type": "none"},
		"thinking":    map[string]any{"type": "disabled"},
	}
	out, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	if out["tool_choice"] != "none" {
		t.Fatalf("tool_choice none mapped to %v", out["tool_choice"])
	}
	if out["temperature"] != 0.3 {
		t.Fatal("temperature dropped")
	}
	if !thinkingDisabled(out) {
		t.Fatal("thinking disabled not propagated")
	}
}

func TestAnthropicStreamParallelToolUse(t *testing.T) {
	rec := httptest.NewRecorder()
	as := &anthropicStream{w: rec, msgID: "m", model: "x"}
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"a","arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"b","arguments":"{\"y\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
	}
	for _, f := range frames {
		var c map[string]any
		if err := json.Unmarshal([]byte(f), &c); err != nil {
			t.Fatal(err)
		}
		if err := as.onChunk(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := as.finish(&relayResult{FinishReason: "tool_calls"}); err != nil {
		t.Fatal(err)
	}
	// 每个块的 partial_json 拼起来必须是合法 JSON
	parts := map[float64]string{}
	ids := map[float64]string{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev)
		idx, _ := ev["index"].(float64)
		switch ev["type"] {
		case "content_block_start":
			cb := ev["content_block"].(map[string]any)
			ids[idx] = cb["id"].(string)
		case "content_block_delta":
			d := ev["delta"].(map[string]any)
			parts[idx] += d["partial_json"].(string)
		}
	}
	for idx, js := range parts {
		var v any
		if err := json.Unmarshal([]byte(js), &v); err != nil {
			t.Fatalf("block %v has invalid JSON %q (ids=%v)", idx, js, ids)
		}
	}
	if ids[0] != "c1" && ids[1] != "c1" {
		t.Fatalf("tool ids lost: %v", ids)
	}
	if len(ids) != 2 {
		t.Fatalf("parallel tool calls collapsed into %d block(s)", len(ids))
	}
}
