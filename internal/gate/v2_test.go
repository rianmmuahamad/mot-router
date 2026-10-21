package gate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Recognizable identifier implementation published by @opencode/schema 2.0.29.
const v2IdentifierFixture = `const length = 26;
const current = BigInt(timestamp) * 0x1000n + BigInt(counter);
const value = descending ? ~current : current;
const time = Array.from({ length: 6 }, (_, index) => Number((value >> BigInt(40 - 8 * index)) & 0xffn).toString(16).padStart(2, "0")).join("");
const bytes = crypto.getRandomValues(new Uint8Array(length - 12));`

func identifierArchive(t *testing.T, source string) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	w := tar.NewWriter(z)
	if err := w.WriteHeader(&tar.Header{Name: "package/dist/identifier.js", Mode: 0644, Size: int64(len(source))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(source)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	data := b.Bytes()
	h := sha512.Sum512(data)
	return data, "sha512-" + base64.StdEncoding.EncodeToString(h[:])
}

func TestV2ReleaseUsesNewPackageAndVerifiedPublishedSchema(t *testing.T) {
	archive, integrity := identifierArchive(t, v2IdentifierFixture)
	schema, _ := json.Marshal(map[string]any{"name": "@opencode/schema", "version": "2.0.29", "dist": map[string]string{"tarball": "https://registry.npmjs.org/@opencode/schema/-/schema-2.0.29.tgz", "integrity": integrity}})
	visited := []string{}
	hint, err := loadOfficialHints(context.Background(), func(url string) ([]byte, error) {
		visited = append(visited, url)
		switch url {
		case "https://registry.npmjs.org/@opencode/cli/latest":
			return []byte(`{"name":"@opencode/cli","version":"2.0.29"}`), nil
		case "https://registry.npmjs.org/@opencode/schema/2.0.29":
			return schema, nil
		case "https://registry.npmjs.org/@opencode/schema/-/schema-2.0.29.tgz":
			return archive, nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", url)
		}
	})
	if err != nil || hint.ClientFormat != ClientV2 || hint.Version != "2.0.29" || hint.Identity != DefaultIdentity() || hint.UserAgent != "opencode/latest/2.0.29/cli" || len(visited) != 3 {
		t.Fatalf("invalid V2 metadata result: %+v %v %v", hint, err, visited)
	}
	corrupt := append([]byte(nil), archive...)
	corrupt[0] ^= 1
	if _, err := verifiedIdentifier(corrupt, integrity); err == nil {
		t.Fatal("tampered archive accepted")
	}
	for _, code := range []string{strings.ReplaceAll(v2IdentifierFixture, "0x1000n", "1234n"), strings.ReplaceAll(v2IdentifierFixture, "length: 6", "length: 8"), strings.ReplaceAll(v2IdentifierFixture, "descending ? ~current : current", "current"), strings.ReplaceAll(v2IdentifierFixture, "length = 26", "length = 1000000")} {
		if _, err := ParseOfficialIdentity(code); err == nil {
			t.Fatal("unsupported V2 source accepted")
		}
	}
}

func TestUnknownV2MajorOrSchemaURLNeverRunsOrDowngrades(t *testing.T) {
	for _, first := range []string{`{"name":"@opencode/cli","version":"3.0.0"}`, `{"name":"other","version":"2.0.29"}`} {
		reads := 0
		_, err := loadOfficialHints(context.Background(), func(url string) ([]byte, error) { reads++; return []byte(first), nil })
		if err == nil || reads != 1 {
			t.Fatal("unrecognized release silently downgraded")
		}
	}
	reads := 0
	_, err := loadOfficialHints(context.Background(), func(url string) ([]byte, error) {
		reads++
		if reads == 1 {
			return []byte(`{"name":"@opencode/cli","version":"2.0.29"}`), nil
		}
		return []byte(`{"name":"@opencode/schema","version":"2.0.29","dist":{"tarball":"https://attacker.invalid/package.tgz"}}`), nil
	})
	if err == nil || reads != 2 {
		t.Fatal("untrusted tarball URL was followed")
	}
}

func TestReferenceVersionDoesNotSilentlyFollowNpmLatest(t *testing.T) {
	archive, integrity := identifierArchive(t, v2IdentifierFixture)
	schema, _ := json.Marshal(map[string]any{"name": "@opencode/schema", "version": NativeV2ReferenceVersion, "dist": map[string]string{"tarball": "https://registry.npmjs.org/@opencode/schema/-/schema-2.0.29.tgz", "integrity": integrity}})
	reads := 0
	hint, err := loadOfficialHints(t.Context(), func(url string) ([]byte, error) {
		reads++
		switch url {
		case "https://registry.npmjs.org/@opencode/cli/2.0.29":
			return []byte(`{"name":"@opencode/cli","version":"2.0.29"}`), nil
		case "https://registry.npmjs.org/@opencode/schema/2.0.29":
			return schema, nil
		case "https://registry.npmjs.org/@opencode/schema/-/schema-2.0.29.tgz":
			return archive, nil
		default:
			t.Fatalf("reference followed a different release: %s", url)
			return nil, nil
		}
	}, NativeV2ReferenceVersion)
	if err != nil || hint.Version != NativeV2ReferenceVersion || reads != 3 {
		t.Fatal("reference version was not used", err)
	}
	for _, version := range []string{"../latest", "3.0.0", "2.0.29\r\n"} {
		if _, err := loadOfficialHints(t.Context(), func(string) ([]byte, error) { t.Fatal("invalid reference fetched"); return nil, nil }, version); err == nil {
			t.Fatal("invalid reference accepted")
		}
	}
}

func TestV2RecoveryRequiresTypedBodyAndReusesWireFormat(t *testing.T) {
	// This fixture asserts the Responses payload shape (developer-role input),
	// so the probe must target the Responses lane.
	t.Setenv("ZEN_RESPONSES_MODELS", "zen-fixture-free")
	ua := "opencode/latest/2.0.29/opencode"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
			Input  []struct {
				Role string `json:"role"`
			} `json:"input"`
			Tools []struct {
				Name       string `json:"name"`
				Parameters struct {
					Properties map[string]any `json:"properties"`
					Required   []string       `json:"required"`
				} `json:"parameters"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		tools := map[string]bool{}
		for _, tool := range body.Tools {
			if tool.Name == "shell" {
				tools["shell"] = tool.Parameters.Properties["command"] != nil && len(tool.Parameters.Required) > 0
			}
			if tool.Name == "read" {
				tools["read"] = tool.Parameters.Properties["path"] != nil && len(tool.Parameters.Required) > 0
			}
		}
		if r.UserAgent() != ua || r.Header.Get("x-opencode-client") != "opencode" || r.Header.Get("x-opencode-request") != "" || r.Header.Get("x-session-affinity") != r.Header.Get("x-opencode-session") || !timestampValid(r.Header.Get("x-opencode-session"), true, 12) || !body.Stream || len(body.Input) < 2 || body.Input[0].Role != "developer" || !tools["read"] {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"type":"FreeTierError"}}`))
			return
		}
		w.Write([]byte(testProbeCompletion))
	}))
	defer upstream.Close()
	p := NewProber(upstream.URL, "zen-fixture-free")
	p.Resolve = func(context.Context) (*OfficialHints, error) {
		return &OfficialHints{Version: "2.0.29", ClientFormat: ClientV2, UserAgent: ua, Identity: DefaultIdentity()}, nil
	}
	profile := p.Probe(context.Background())
	if profile.Source != "probe" || profile.Verdict != "ok" || profile.ClientFormat != ClientV2 || profile.VerifiedVersion != "2.0.29" || profile.MinVersion != "" || !contains(profile.RequiredTools, "read") {
		t.Fatalf("V2 not verified: %+v", profile)
	}
	c := NewCache(p, time.Hour)
	if !c.Restore(profile) {
		t.Fatal("verified V2 profile could not be restored")
	}
	broken := *profile
	broken.ClientFormat = "unknown"
	if c.Restore(&broken) {
		t.Fatal("unknown profile format restored")
	}
	for _, sample := range []struct{ ua, version string }{{"opencode/1.19.0 ai-sdk/4", "1.19.0"}, {ua, "2.0.29"}} {
		if versionFromUA(sample.ua) != sample.version {
			t.Fatal("wrong version parser")
		}
	}
}
