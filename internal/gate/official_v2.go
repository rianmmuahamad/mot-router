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
	"io"
	"math/bits"
	"regexp"
	"strconv"
	"strings"
)

const maxOfficialDownload = 2 << 20

type officialRelease struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

func decodeRelease(body []byte, name string) (officialRelease, error) {
	var release officialRelease
	if len(body) > 65536 || json.Unmarshal(body, &release) != nil || release.Name != name || !versionPattern.MatchString(release.Version) {
		return release, fmt.Errorf("invalid official %s metadata", name)
	}
	return release, nil
}

// V2 moved to @opencode/cli. Its identifier implementation is published in
// @opencode/schema, so recovery no longer relies on a removed GitHub V1 path.
// Read only a recognized schema layout; never install or evaluate package code.
func loadOfficialHints(ctx context.Context, read func(string) ([]byte, error), versions ...string) (*OfficialHints, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	releaseTag := "latest"
	if len(versions) > 0 && versions[0] != "" {
		if !versionPattern.MatchString(versions[0]) || !strings.HasPrefix(versions[0], "2.") {
			return nil, fmt.Errorf("invalid reference client version")
		}
		releaseTag = versions[0]
	}
	metadata, err := read("https://registry.npmjs.org/@opencode/cli/" + releaseTag)
	if err == nil {
		release, e := decodeRelease(metadata, "@opencode/cli")
		if e != nil {
			return nil, e
		}
		if !strings.HasPrefix(release.Version, "2.") {
			return nil, fmt.Errorf("unsupported OpenCode major version %s", release.Version)
		}
		if releaseTag != "latest" && release.Version != releaseTag {
			return nil, fmt.Errorf("reference client version mismatch")
		}
		schemaMeta, e := read("https://registry.npmjs.org/@opencode/schema/" + release.Version)
		if e != nil {
			return nil, e
		}
		schema, e := decodeRelease(schemaMeta, "@opencode/schema")
		if e != nil {
			return nil, e
		}
		expected := "https://registry.npmjs.org/@opencode/schema/-/schema-" + release.Version + ".tgz"
		if schema.Version != release.Version || schema.Dist.Tarball != expected {
			return nil, fmt.Errorf("official schema version or URL mismatch")
		}
		archive, e := read(expected)
		if e != nil {
			return nil, e
		}
		source, e := verifiedIdentifier(archive, schema.Dist.Integrity)
		if e != nil {
			return nil, e
		}
		identity, e := ParseOfficialIdentity(source)
		if e != nil {
			return nil, e
		}
		return &OfficialHints{Version: release.Version, Identity: identity, ClientFormat: ClientV2,
			UserAgent: "opencode/latest/" + release.Version + "/cli"}, nil
	}
	if releaseTag != "latest" {
		return nil, fmt.Errorf("reference client metadata: %w", err)
	}
	// Preserve the legacy channel if the V2 registry is unavailable. Neither a
	// malformed V2 layout nor a new unsupported major silently downgrades here.
	legacy, legacyErr := read("https://registry.npmjs.org/opencode-ai/latest")
	if legacyErr != nil {
		return nil, fmt.Errorf("V2 metadata: %v; V1 metadata: %w", err, legacyErr)
	}
	release, legacyErr := decodeRelease(legacy, "opencode-ai")
	if legacyErr != nil {
		return nil, legacyErr
	}
	if !strings.HasPrefix(release.Version, "1.") {
		return nil, fmt.Errorf("unsupported legacy release")
	}
	source, legacyErr := read("https://raw.githubusercontent.com/anomalyco/opencode/v" + release.Version + "/packages/opencode/src/id/id.ts")
	if legacyErr != nil {
		return nil, legacyErr
	}
	identity, legacyErr := ParseOfficialIdentity(string(source))
	if legacyErr != nil {
		return nil, legacyErr
	}
	return &OfficialHints{Version: release.Version, Identity: identity, ClientFormat: ClientV1}, nil
}

func verifiedIdentifier(archive []byte, integrity string) (string, error) {
	if len(archive) > maxOfficialDownload || !strings.HasPrefix(integrity, "sha512-") {
		return "", fmt.Errorf("invalid official archive")
	}
	expected, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(integrity, "sha512-"))
	actual := sha512.Sum512(archive)
	if err != nil || !bytes.Equal(expected, actual[:]) {
		return "", fmt.Errorf("official archive integrity mismatch")
	}
	z, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	defer z.Close()
	limited := &io.LimitedReader{R: z, N: 8 << 20}
	t := tar.NewReader(limited)
	for {
		h, err := t.Next()
		if err == io.EOF {
			return "", fmt.Errorf("official identifier source missing")
		}
		if err != nil {
			return "", err
		}
		if h.Name != "package/dist/identifier.js" {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size < 1 || h.Size > 65536 {
			return "", fmt.Errorf("invalid identifier source size/type")
		}
		data, err := io.ReadAll(io.LimitReader(t, 65537))
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

func parseV2Identity(source string) (IdentityPolicy, error) {
	get := func(pattern string) (string, error) {
		m := regexp.MustCompile(pattern).FindStringSubmatch(source)
		if len(m) != 2 {
			return "", fmt.Errorf("unrecognized V2 identifier layout")
		}
		return m[1], nil
	}
	length, err := get(`const\s+length\s*=\s*(\d+)`)
	if err != nil {
		return IdentityPolicy{}, err
	}
	width, err := get(`Array\.from\(\{\s*length:\s*(\d+)\s*\}`)
	if err != nil {
		return IdentityPolicy{}, err
	}
	mult, err := get(`BigInt\(timestamp\)\s*\*\s*(0x[0-9a-fA-F]+|\d+)n\s*\+\s*BigInt\(counter\)`)
	if err != nil {
		return IdentityPolicy{}, err
	}
	if !strings.Contains(source, "descending ? ~current : current") || !strings.Contains(source, "40 - 8 * index") || !strings.Contains(source, "crypto.getRandomValues") || !strings.Contains(source, ".padStart(2, \"0\")") {
		return IdentityPolicy{}, fmt.Errorf("unsupported V2 identifier encoding")
	}
	n, _ := strconv.Atoi(length)
	b, _ := strconv.Atoi(width)
	m, err := strconv.ParseUint(mult, 0, 64)
	if err != nil || m == 0 || m&(m-1) != 0 || b != 6 {
		return IdentityPolicy{}, fmt.Errorf("unsupported V2 timestamp width/counter")
	}
	p := DefaultIdentity()
	p.TimestampBytes = b
	p.CounterBits = bits.TrailingZeros64(m)
	p.SuffixLength = n - 2*b
	if !p.Valid() {
		return IdentityPolicy{}, fmt.Errorf("unsupported V2 identifier parameters")
	}
	return p, nil
}
