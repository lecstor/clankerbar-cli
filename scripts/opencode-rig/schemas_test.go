package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCatalogAcceptsEveryShape(t *testing.T) {
	dir := t.TempDir()
	shapes := map[string]string{
		"envelope.json": `{"source":"plane","tools":[{"name":"a","inputSchema":{}},{"name":"b"}]}`,
		"tools.json":    `{"tools":[{"name":"a"},{"name":"b"}]}`,
		"result.json":   `{"result":{"tools":[{"name":"a"},{"name":"b"}]}}`,
		"bare.json":     `[{"name":"a"},{"name":"b"}]`,
	}
	for name, content := range shapes {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		cat, err := loadCatalog(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := strings.Join(cat.toolNames(), ","); got != "a,b" {
			t.Errorf("%s: tools = %s", name, got)
		}
	}
	if _, err := loadCatalog(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file did not error")
	}
	empty := filepath.Join(dir, "empty.json")
	os.WriteFile(empty, []byte(`{"tools":[]}`), 0o644)
	if _, err := loadCatalog(empty); err == nil {
		t.Error("empty catalog did not error")
	}
}

func TestSaveCatalogPreservesRawToolBytes(t *testing.T) {
	dir := t.TempDir()
	raw := `{"name":"edit_task","inputSchema":{"type":"object","properties":{"priority":{"anyOf":[{"type":"string","enum":["h"]},{"type":"null"}]}}}}`
	cat := catalog{Source: "test", Tools: []json.RawMessage{json.RawMessage(raw)}}
	path := filepath.Join(dir, "schemas.json")
	if err := saveCatalog(path, cat); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"anyOf"`) {
		t.Fatalf("saved catalog re-encoded the schema:\n%s", data)
	}
	back, err := loadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(back.Tools[0]) != raw {
		t.Errorf("round trip changed the tool bytes:\n got %s\nwant %s", back.Tools[0], raw)
	}
}

func TestResponseJSONParsesSSEAndPlainJSON(t *testing.T) {
	sse := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n")
	got, err := responseJSON(sse)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"ok":true`) {
		t.Errorf("sse = %s", got)
	}
	plain, err := responseJSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	if err != nil || !strings.Contains(string(plain), "result") {
		t.Errorf("plain = %s, %v", plain, err)
	}
	// Multi-line data fields are one JSON document, per the SSE spec.
	multi := []byte("data: {\"jsonrpc\":\"2.0\",\n\n")
	if _, err := responseJSON(multi); err == nil {
		t.Error("malformed SSE did not error")
	}
}

// TestFetchCatalogTalksReadOnlyMCP drives the real fetch path against a fake
// Streamable-HTTP MCP endpoint: initialize, notifications/initialized,
// tools/list. The endpoint refuses any tools/call, so a regression that starts
// writing to the plane would fail here.
func TestFetchCatalogTalksReadOnlyMCP(t *testing.T) {
	const key = "test-plane-key"
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		methods = append(methods, msg.Method)
		w.Header().Set("Content-Type", "text/event-stream")
		switch msg.Method {
		case "initialize":
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2025-03-26\",\"capabilities\":{},\"serverInfo\":{\"name\":\"fake\",\"version\":\"0\"}}}\n\n")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[{\"name\":\"next_task\",\"inputSchema\":{\"type\":\"object\"}},{\"name\":\"edit_task\",\"inputSchema\":{\"type\":\"object\"}}]}}\n\n")
		default:
			http.Error(w, "the rig must never call a tool", http.StatusForbidden)
		}
	}))
	defer srv.Close()

	cat, err := fetchCatalog(context.Background(), srv.URL, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cat.toolNames(), ","); got != "next_task,edit_task" {
		t.Errorf("tools = %s", got)
	}
	if cat.Source != srv.URL {
		t.Errorf("source = %q", cat.Source)
	}
	want := []string{"initialize", "notifications/initialized", "tools/list"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Errorf("methods = %v, want %v", methods, want)
	}
}

func TestShippedProposedSchemaLoads(t *testing.T) {
	cat, err := loadCatalog("testdata/schema-proposed.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cat.toolNames(), ","); got != "edit_task,frobnicate" {
		t.Fatalf("tools = %s", got)
	}
	// The example must keep the nullable-enum rendering the schema scenario
	// exists to exercise.
	var edit struct {
		InputSchema struct {
			Properties map[string]struct {
				AnyOf []json.RawMessage `json:"anyOf"`
			} `json:"properties"`
		} `json:"inputSchema"`
	}
	if err := json.Unmarshal(cat.Tools[0], &edit); err != nil {
		t.Fatal(err)
	}
	if len(edit.InputSchema.Properties["priority"].AnyOf) != 2 {
		t.Errorf("priority is no longer a nullable enum: %s", cat.Tools[0])
	}
	if len(edit.InputSchema.Properties["container"].AnyOf) != 2 {
		t.Errorf("container is no longer a nullable boolean: %s", cat.Tools[0])
	}
}

func TestResponseJSONRejectsAnEmptyBody(t *testing.T) {
	if got, err := responseJSON(nil); err != nil || got != nil {
		t.Errorf("empty body = %s, %v", got, err)
	}
}
