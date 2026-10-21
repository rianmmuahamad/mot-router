package gate

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type endpointTransport func(*http.Request) (*http.Response, error)

func (f endpointTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOfficialEndpointMigrationIsVerifiedAndPersisted(t *testing.T) {
	// The fixture only accepts the Responses path, so pin the probe to that lane.
	t.Setenv("ZEN_RESPONSES_MODELS", "zen-fixture-free")
	p := NewProber(LegacyBaseURL, "zen-fixture-free")
	p.Resolve = func(context.Context) (*OfficialHints, error) { return nil, nil }
	acceptedRequests := 0
	p.HTTP = &http.Client{Transport: endpointTransport(func(r *http.Request) (*http.Response, error) {
		status := 403
		body := `{"error":{"type":"FreeTierError"}}`
		if r.URL.String() == DefaultBaseURL+"/responses" {
			status = 200
			body = testProbeCompletion
			acceptedRequests++
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	c := NewCache(p, time.Hour)
	profile := c.Refresh(context.Background())
	if profile.Verdict != "ok" || profile.Source != "probe" || profile.BaseURL != DefaultBaseURL || !strings.Contains(profile.RepairSource, "official endpoint") || acceptedRequests < 3 || c.Automatic().Repairs != 1 {
		t.Fatalf("endpoint not verified: %+v requests=%d", profile, acceptedRequests)
	}
	restored := NewCache(NewProber(LegacyBaseURL, "zen-fixture-free"), time.Hour)
	if !restored.Restore(profile) || restored.Snapshot().BaseURL != DefaultBaseURL {
		t.Fatal("verified endpoint was not restored")
	}
	bad := *profile
	bad.BaseURL = "https://attacker.invalid"
	if restored.Restore(&bad) {
		t.Fatal("arbitrary saved endpoint accepted")
	}
}

func TestUnverifiedOfficialEndpointKeepsCurrentURL(t *testing.T) {
	t.Setenv("ZEN_RESPONSES_MODELS", "zen-fixture-free")
	p := NewProber(LegacyBaseURL, "zen-fixture-free")
	p.Resolve = func(context.Context) (*OfficialHints, error) { return nil, nil }
	p.HTTP = &http.Client{Transport: endpointTransport(func(r *http.Request) (*http.Response, error) {
		status := 403
		body := `{"error":{"type":"FreeTierError"}}`
		if r.URL.String() == DefaultBaseURL+"/responses" {
			status = 429
			body = `{"error":{"type":"FreeUsageLimitError"}}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})}
	profile := p.Probe(context.Background())
	if profile.Source == "probe" || profile.BaseURL != LegacyBaseURL {
		t.Fatalf("unverified migration accepted: %+v", profile)
	}
}
