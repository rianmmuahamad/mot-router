// audit_router_compat exercises the router against a loopback fixture only.
// It reports observed compatibility gaps without changing production behavior.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"

	"github.com/rian/mot-router/internal/gate"
	"github.com/rian/mot-router/internal/translate"
	"github.com/rian/mot-router/internal/zen"
)

func main() {
	var request translate.OpenAIChatRequest
	const input = `{"model":"deepseek-v4.1-flash-free","max_completion_tokens":128,"stream":true,"temperature":0.2,"top_p":0.8,"tool_choice":"none","parallel_tool_calls":false,"stop":["END"],"response_format":{"type":"json_object"},"reasoning_effort":"low","messages":[{"role":"system","content":"Local audit fixture only."},{"role":"user","content":[{"type":"text","text":"Describe this fixture."},{"type":"image_url","image_url":{"url":"https://fixture.invalid/image.png"}}]}]}`
	if err := json.Unmarshal([]byte(input), &request); err != nil {
		panic(err)
	}
	profile := &zen.GateProfile{ClientFormat: "v2"}
	translated, err := translate.OpenAIToZen(&request, request.Model, profile)
	if err != nil {
		panic(err)
	}
	body := zen.ConvertToChatPayload(request.Model, translated.Body)
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	missing := []string{}
	for _, key := range []string{"temperature", "top_p", "parallel_tool_calls", "stop", "response_format", "reasoning_effort"} {
		if _, present := body[key]; !present {
			missing = append(missing, key)
		}
	}
	toolChoice, present := body["tool_choice"]
	var errorReport map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"type\":\"FixtureError\",\"message\":\"LOCAL_AUDIT_REJECTION\"}}\n\n")
	}))
	defer upstream.Close()
	client := zen.New(upstream.URL+"/v1", "local-audit-fixture", "")
	events, status, err := client.StreamChatCompletions(context.Background(), request.Model, "audit-fixture", translated.Body)
	if err != nil {
		panic(err)
	}
	collected := []zen.Event{}
	for event := range events {
		collected = append(collected, event)
	}
	replay := make(chan zen.Event, len(collected))
	interrupted := false
	for _, event := range collected {
		interrupted = interrupted || event.StreamInterrupted
		replay <- event
	}
	close(replay)
	errorReport = map[string]any{"upstream_http_status": status, "event_count": len(collected),
		"interrupted_flag": interrupted, "translated_response": translate.CollectOpenAI(replay, request.Model, translated.ToolMap)}
	probe := gate.NewProber(upstream.URL+"/v1", request.Model).Probe(context.Background())
	baseline := map[string]any{"verdict": probe.Verdict, "source": probe.Source}
	for _, condition := range probe.Conditions {
		if condition.Name == "baseline" || condition.Name == "profile_verified" {
			baseline[condition.Name] = map[string]any{"ok": condition.OK, "status": condition.Status}
		}
	}
	result := map[string]any{
		"mode": "synthetic loopback fixture; no provider calls; no production changes",
		"payload": map[string]any{"missing_fields": missing, "tool_choice_present": present,
			"tool_choice_actual": toolChoice, "tool_choice_requested": "none", "outbound_keys_before_wire_parameter_rename": keys,
			"messages": body["messages"]},
		"sse_error": errorReport,
		"sse_error_baseline_probe": baseline,
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		panic(err)
	}
}
