package gate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCurrentV2ContractIsPreferredOverSuccessfulLegacySeed(t *testing.T) {
	for _, status := range []int{200, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			hits := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if r.UserAgent() != "opencode/latest/2.0.29/cli" || r.Header.Get("x-opencode-request") != "" {
					t.Error("legacy seed prevented V2 check or caused downgrade")
				}
				w.WriteHeader(status)
				if status == 200 {
					w.Write([]byte(testProbeCompletion))
				} else {
					w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
				}
			}))
			defer upstream.Close()
			p := NewProber(upstream.URL, "glm-5.3-flash-free")
			p.Resolve = func(context.Context) (*OfficialHints, error) {
				return &OfficialHints{Version: "2.0.29", ClientFormat: ClientV2, UserAgent: "opencode/latest/2.0.29/cli", Identity: DefaultIdentity()}, nil
			}
			cache := NewCache(p, time.Hour)
			old := Fallback()
			old.Source, old.Verdict, old.ProbeModel = "probe", "ok", DefaultProbeModel
			cache.Restore(old)
			result := cache.RefreshModel(t.Context(), "glm-5.3-flash-free")
			if status == 200 {
				if hits != 2 || result.ClientFormat != ClientV2 || result.ProbeModel != "glm-5.3-flash-free" {
					t.Fatal("current contract was not verified and selected")
				}
			} else if hits != 1 || result.Source != "fallback" || result.UserAgent != old.UserAgent {
				t.Fatal("denied V2 was retried, falsely verified, or replaced last-good settings")
			}
		})
	}
}

func TestOfficialV2IsVerifiedBeforeLegacyEndpointCanStopRecovery(t *testing.T) {
	p := NewProber(DefaultBaseURL, "")
	ua := "opencode/latest/2.0.29/cli"
	p.Resolve = func(context.Context) (*OfficialHints, error) {
		return &OfficialHints{Version: "2.0.29", ClientFormat: ClientV2, UserAgent: ua, Identity: DefaultIdentity()}, nil
	}
	requests := []string{}
	p.HTTP = &http.Client{Transport: endpointTransport(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String()+" "+r.UserAgent())
		status, body := 403, `{"error":{"type":"FreeTierError"}}`
		if strings.HasPrefix(r.URL.String(), LegacyBaseURL) {
			status, body = 429, `{"error":{"type":"FreeUsageLimitError"}}`
		}
		if r.UserAgent() == ua && r.URL.String() == DefaultBaseURL+"/chat/completions" {
			status, body = 200, testProbeCompletion
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	current := Fallback()
	current.BaseURL = DefaultBaseURL
	profile, _, _ := p.recoverBaseline(t.Context(), current)
	if profile == nil || profile.ClientFormat != ClientV2 || len(requests) != 2 {
		t.Fatalf("V2 blocked by historical endpoint: %+v %v", profile, requests)
	}
	for _, r := range requests {
		if !strings.Contains(r, ua) || strings.Contains(r, LegacyBaseURL) {
			t.Fatal(requests)
		}
	}
}

func TestDeepSeekProbeUsesChatEndpointAndBoundedPayload(t *testing.T) {
	for _, format := range []string{ClientV1, ClientV2} {
		t.Run(format, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var b map[string]any
				if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
					t.Fatal(err)
				}
				if r.URL.Path != "/chat/completions" || b["model"] != DefaultProbeModel || b["input"] != nil || b["stream"] != true {
					t.Errorf("wrong probe endpoint/body: %s %+v", r.URL.Path, b)
				}
				messages := b["messages"].([]any)
				if messages[0].(map[string]any)["role"] != "system" || len(messages) != 2 {
					t.Error("wrong chat roles")
				}
				key := "max_tokens"
				if format == ClientV2 {
					key = "max_completion_tokens"
				}
				if b[key] != float64(16) {
					t.Errorf("unbounded probe: %+v", b)
				}
				names := map[string]map[string]any{}
				for _, item := range b["tools"].([]any) {
					tool := item.(map[string]any)
					if tool["type"] != "function" || tool["name"] != nil {
						t.Error("Responses tool sent to Chat endpoint")
					}
					fn := tool["function"].(map[string]any)
					names[fn["name"].(string)] = fn["parameters"].(map[string]any)
				}
				if format == ClientV2 {
					if len(names) != len(nativeToolNames(ClientV2)) || names["read"]["required"].([]any)[0] != "path" {
						t.Error("V2 capture schema mismatch")
					}
				}
				w.Write([]byte(testProbeCompletion))
			}))
			defer upstream.Close()
			p := NewProber(upstream.URL, "")
			profile := Fallback()
			profile.ClientFormat = format
			p.setProbeProfile(profile)
			if v := p.sendOnce(t.Context(), profile.UserAgent, "fixture", p.probeTools(), true); !accepted(v) {
				t.Fatalf("probe failed: %+v", v)
			}
		})
	}
}

func TestDeepSeekV2RecoveryVerifiesChatToolsAndCLIHeaders(t *testing.T) {
	ua := "opencode/latest/2.0.29/cli"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		names := map[string]bool{}
		for _, tool := range body.Tools {
			names[tool.Function.Name] = true
		}
		if r.URL.Path != "/chat/completions" || r.UserAgent() != ua || r.Header.Get("x-opencode-client") != "cli" || r.Header.Get("x-opencode-request") != "" || !timestampValid(r.Header.Get("x-opencode-session"), true, 12) || !body.Stream || !names["read"] {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
			return
		}
		w.Write([]byte(testProbeCompletion))
	}))
	defer upstream.Close()
	p := NewProber(upstream.URL, "")
	p.Resolve = func(context.Context) (*OfficialHints, error) {
		return &OfficialHints{Version: "2.0.29", ClientFormat: ClientV2, UserAgent: ua, Identity: DefaultIdentity()}, nil
	}
	profile := p.Probe(t.Context())
	if profile.Source != "probe" || profile.Verdict != "ok" || profile.ClientFormat != ClientV2 || profile.UserAgent != ua {
		t.Fatalf("chat migration failed: %+v", profile)
	}
}
