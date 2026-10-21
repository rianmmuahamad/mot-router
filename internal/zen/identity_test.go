package zen

import (
	"strings"
	"testing"

	"github.com/rian/mot-router/internal/gate"
)

func TestHeadersShareCanonicalIdentityWithProber(t *testing.T) {
	client := New("", "", "")
	a := client.Headers("muse-spark-1.3-contributor-free", "conversation")
	b := client.Headers("muse-spark-1.3-contributor-free", "conversation")
	if a["User-Agent"] != gate.DefaultUserAgent || !strings.Contains(a["User-Agent"], "1.19.0") {
		t.Fatal("stale client version")
	}
	if a["x-opencode-session"] != gate.SessionID("conversation", "ses_", 12, 14) || a["x-opencode-session"] != b["x-opencode-session"] || a["x-opencode-session-id"] != a["x-opencode-session"] {
		t.Fatal("session identities disagree")
	}
	if !strings.HasPrefix(a["x-opencode-request"], "msg_") || len(a["x-opencode-request"]) != 30 || a["x-opencode-request"] == b["x-opencode-request"] {
		t.Fatal("missing or reused request identifier")
	}
}
