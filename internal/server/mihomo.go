package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rian/mot-router/internal/mihomo"
	"github.com/rian/mot-router/internal/zen"
)

// mihomoMu serializes node switching. Concurrent switches would race on the
// single "now" pointer and could leave the selection in an unknown node.
var mihomoMu sync.Mutex

// Settings keys for the mihomo integration.
const (
	settingMihomoEnabled    = "mihomo_enabled"
	settingMihomoController = "mihomo_controller"
	settingMihomoProxy      = "mihomo_proxy"
	settingMihomoSecret     = "mihomo_secret"
	settingMihomoGroup      = "mihomo_group"
	settingMihomoRotate     = "mihomo_rotate_on_limit"
)

// defaultMihomoController and defaultMihomoProxy are the conventional ports for
// a stock mihomo install.
const (
	defaultMihomoController = "http://127.0.0.1:9090"
	defaultMihomoProxy      = "http://127.0.0.1:7890"
)

// mihomoClient returns the shared client for the stored settings.
//
// The client must be long-lived, not built per request: it owns the pooled HTTP
// transport, and a fresh one per call leaks a connection pool every time. The
// client is rebuilt only when the endpoint settings actually change.
func (s *Server) mihomoClient() *mihomo.Client {
	controller := s.store.GetSetting(settingMihomoController)
	if controller == "" {
		controller = defaultMihomoController
	}
	proxy := s.store.GetSetting(settingMihomoProxy)
	if proxy == "" {
		proxy = defaultMihomoProxy
	}
	secret := s.store.GetSetting(settingMihomoSecret)
	if nodeBankRuntimeEnabled() {
		controller, proxy, secret = "http://127.0.0.1:9093", "http://127.0.0.1:7893", ""
	}

	s.mihomoMu.Lock()
	defer s.mihomoMu.Unlock()
	if s.mihomoCached != nil && s.mihomoKey == controller+"|"+proxy+"|"+secret {
		return s.mihomoCached
	}
	// Endpoint changed: drop the old pool before replacing the client.
	if s.mihomoCached != nil {
		s.mihomoCached.ResetConnections()
	}
	s.mihomoCached = mihomo.New(controller, proxy, secret)
	s.mihomoKey = controller + "|" + proxy + "|" + secret
	return s.mihomoCached
}

func nodeBankRuntimeEnabled() bool { return os.Getenv("MOT_NODEBANK_RUNTIME") == "1" }

// mihomoEnabled reports whether requests should be routed through the proxy.
func (s *Server) mihomoEnabled() bool {
	return s.store.GetSetting(settingMihomoEnabled) == "1"
}

// mihomoRotateOnLimit reports whether a rate limit should trigger a node switch.
func (s *Server) mihomoRotateOnLimit() bool {
	// Default on: rotating is the whole point of the integration.
	return s.store.GetSetting(settingMihomoRotate) != "0"
}

// mihomoGroup returns the configured group, or the first Selector group.
func (s *Server) mihomoGroup(ctx context.Context, c *mihomo.Client) (string, error) {
	if nodeBankRuntimeEnabled() {
		return "MOT", nil
	}
	if g := s.store.GetSetting(settingMihomoGroup); g != "" {
		return g, nil
	}
	groups, err := c.SelectorGroups(ctx)
	if err != nil {
		return "", err
	}
	if len(groups) == 0 {
		return "", fmt.Errorf("mihomo tidak punya group bertipe Selector; " +
			"tambahkan satu di config atau pilih group lain")
	}
	// Prefer a group with many nodes: it gives rotation something to work with.
	best := groups[0]
	for _, g := range groups {
		if len(g.Nodes()) > len(best.Nodes()) {
			best = g
		}
	}
	return best.Name, nil
}

// isRegionBlocked reports whether an upstream failure is a geo restriction
// rather than a rate limit.
//
// Zen answers a request from an unsupported country with
// `403 RegionError: This model is not available in your region`, which is a
// different problem from a rate limit but has the same fix: switch nodes.
func isRegionBlocked(status int, detail string) bool {
	if status != http.StatusForbidden {
		return false
	}
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "regionerror") ||
		strings.Contains(lower, "not available in your region") ||
		strings.Contains(lower, "region")
}

// isTransportFailure reports whether an error means the request never got a
// usable answer from upstream.
//
// A proxy exit node can drop a connection mid-request, surfacing as
// "unexpected EOF". That is intermittent per node, so it must trigger a rotation
// like a rate limit does — otherwise the client sees a 502 for a condition a
// different node would have served.
func isTransportFailure(status int, detail string) bool {
	// No HTTP response at all means the connection never completed.
	if status == 0 {
		return true
	}
	// A server that answered with a 4xx or 5xx did respond; whatever the body
	// says, this is not a transport failure.
	if status >= 400 {
		return false
	}

	// Only the phrases Go's net/http actually emits are matched, and the bare
	// token "eof" is deliberately absent: as a substring it appears inside
	// unrelated words and would misclassify a validation error as a dropped
	// connection.
	lower := strings.ToLower(detail)
	for _, needle := range []string{
		"unexpected eof",
		"eof", // exact token, checked below
		"connection reset",
		"broken pipe",
		"connection refused",
		"no route to host",
		"i/o timeout",
		"tls handshake",
		"server closed",
	} {
		if needle == "eof" {
			if hasWord(lower, "eof") {
				return true
			}
			continue
		}
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// hasWord reports whether s contains word as a standalone token.
//
// Substring matching is too loose here: "eof" would match "eof-ish" text in an
// unrelated error message.
func hasWord(s, word string) bool {
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i:i+len(word)] != word {
			continue
		}
		beforeOK := i == 0 || !isWordByte(s[i-1])
		afterOK := i+len(word) == len(s) || !isWordByte(s[i+len(word)])
		if beforeOK && afterOK {
			return true
		}
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// isUpstreamOverloaded reports whether the upstream is temporarily saturated.
//
// Zen answers with `service_overloaded` when its own backend is under load. The
// condition is transient, so retrying — ideally on another node — beats
// surfacing a 503 to the client.
func isUpstreamOverloaded(status int, detail string) bool {
	if status == http.StatusServiceUnavailable || status == http.StatusBadGateway {
		return true
	}
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "service_overloaded") ||
		strings.Contains(lower, "temporarily overloaded")
}

// shouldRotate reports whether a failure is worth retrying on another node.
//
// Rotation helps for anything tied to the exit path or a transient upstream
// condition: a per-IP rate limit, a region block, a dropped connection, or a
// saturated backend.
func shouldRotate(status int, detail string) bool {
	// Usage and region policies are not evidence of a broken exit path.
	if isRateLimited(status, detail) || status == 401 || status == 403 {
		return false
	}
	return isTransportFailure(status, detail) || isUpstreamOverloaded(status, detail)
}

// Policy denials never justify an IP penalty or an IP switch.
func ipTiedFailure(status int, detail string) bool { return false }

// leafNodes filters a group's entries down to usable proxy nodes.
//
// Four kinds of entry are excluded:
//   - DIRECT / REJECT, which are not upstream servers;
//   - references to other groups, whose choice is made by that group;
//   - pseudo-entries a subscription injects to carry account notices
//     ("traffic remaining: 0", "expires on …"). These look like real Vless
//     nodes but always report alive=false with a zero delay;
//   - nodes whose health check has not produced a positive delay.
//
// The result is sorted by measured latency, fastest first, so a rotation that
// picks the next entry tends to land on a responsive node.
func leafNodes(g mihomo.Group, all map[string]mihomo.Group) []string {
	type entry struct {
		name  string
		delay int
	}
	entries := make([]entry, 0, len(g.All))

	for _, name := range g.All {
		// REJECT is never a usable exit. DIRECT is: a group may legitimately
		// route through the host's own connection, and the VPS deployment does
		// exactly that because the IP works where WARP is rate limited.
		//
		// Excluding DIRECT here was a real bug: a group of DIRECT + WARP reduced
		// to just WARP, so rotation kept selecting the same limited node —
		// observed in the log as `rotate: from="WARP" to="WARP"`.
		if name == "REJECT" {
			continue
		}
		info, known := all[name]
		if !known {
			if name == "DIRECT" {
				entries = append(entries, entry{name: name, delay: 0})
			}
			continue
		}
		// But they are valid selectable leaf proxy exits.
		// A group reference decides its own node.
		switch info.Type {
		case "Selector", "URLTest", "Fallback", "LoadBalance", "Relay":
			continue
		}
		// DIRECT reports no history, and Healthy() requires a positive delay.
		if name == "DIRECT" {
			entries = append(entries, entry{name: name, delay: 0})
			continue
		}
		if !info.Healthy() {
			continue
		}
		entries = append(entries, entry{name: name, delay: info.Delay()})
	}

	sort.SliceStable(entries, func(i, j int) bool { return entries[i].delay < entries[j].delay })

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.name)
	}
	return out
}

// mihomoStatus is the dashboard view.
type mihomoStatus struct {
	NativeStatus map[string]any `json:"native_status,omitempty"`
	Enabled      bool           `json:"enabled"`
	Reachable    bool           `json:"reachable"`
	Version      string         `json:"version"`
	Group        string         `json:"group"`
	Node         string         `json:"node"`
	EgressIP     string         `json:"egress_ip"`
	Rotate       bool           `json:"rotate_on_limit"`
	// Controller and Proxy are the endpoints in use, so the dashboard can show
	// the configured values instead of guessing.
	Controller string `json:"controller"`
	Proxy      string `json:"proxy"`
	Error      string `json:"error,omitempty"`
	// QuotaExhausted marks a subscription that ran out or expired, so the
	// dashboard can say so instead of reporting a vague connectivity failure.
	QuotaExhausted bool `json:"quota_exhausted,omitempty"`
	// Groups lists the selectable groups with their node counts.
	Groups []mihomoGroupView `json:"groups"`
	// NodeBankGeneratedAt describes the laptop snapshot, not a VPS health check.
	NodeBankGeneratedAt int64 `json:"nodebank_generated_at,omitempty"`
}

type mihomoGroupView struct {
	ValidatedIPs     []string `json:"validated_ips,omitempty"`
	Name             string   `json:"name"`
	Now              string   `json:"now"`
	Nodes            int      `json:"nodes"`
	Selectable       bool     `json:"selectable"`
	NodeBankIPs      []string `json:"nodebank_ips,omitempty"`
	NodeBankReceived int64    `json:"nodebank_received_at,omitempty"`
}

func withNodeBankCandidates(groups []mihomoGroupView, ips []string, received int64) []mihomoGroupView {
	if len(ips) == 0 {
		return groups
	}
	for i := range groups {
		if strings.EqualFold(groups[i].Name, "MOT") {
			groups[i].NodeBankIPs = ips
			groups[i].NodeBankReceived = received
			return groups
		}
	}
	// Keep the candidates visible under the requested MOT group even when the
	// active proxy runtime does not yet have a selector with that name. It is
	// deliberately non-selectable and has zero healthy runtime nodes.
	return append(groups, mihomoGroupView{
		Name: "MOT", Now: "belum aktif di runtime", Selectable: false,
		NodeBankIPs: ips, NodeBankReceived: received,
	})
}

// handleMihomoStatus reports the integration state.
func (s *Server) handleMihomoStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	c := s.mihomoClient()
	st := mihomoStatus{
		Enabled:    s.mihomoEnabled(),
		Rotate:     s.mihomoRotateOnLimit(),
		Groups:     []mihomoGroupView{},
		Controller: c.ControllerURL,
		Proxy:      c.ProxyURL,
	}
	bankIPs, bankReceived, bankErr := nodeBankIPs(s.dataDir)
	if bankErr == nil {
		st.NodeBankGeneratedAt = bankReceived
	}

	version, err := c.Version(ctx)
	if err != nil {
		st.Error = err.Error()
		st.Groups = withNodeBankCandidates(st.Groups, bankIPs, bankReceived)
		writeJSON(w, http.StatusOK, st)
		return
	}
	st.Reachable = true
	st.Version = version
	native := strings.HasPrefix(version, "nodebank-native/")
	if native {
		st.NativeStatus, _ = c.RuntimeStatus(ctx)
		bankIPs = nil
	}

	all, err := c.AllProxies(ctx)
	if err == nil {
		groups, _ := c.SelectorGroups(ctx)
		if native && len(groups) == 0 {
			if mot, ok := all["MOT"]; ok {
				groups = append(groups, mot)
			}
		}
		for _, g := range groups {
			// Count real proxies only: a group's list can include references to
			// other groups, which are not switchable egress targets.
			view := mihomoGroupView{
				Name: g.Name, Now: g.Now, Nodes: len(leafNodes(g, all)), Selectable: true,
			}
			if native {
				seen := map[string]bool{}
				for _, name := range leafNodes(g, all) {
					if ip := all[name].ExitIP; name != "DIRECT" && ip != "" && !seen[ip] {
						view.ValidatedIPs = append(view.ValidatedIPs, ip)
						seen[ip] = true
					}
				}
			}
			st.Groups = append(st.Groups, view)
		}
	}
	st.Groups = withNodeBankCandidates(st.Groups, bankIPs, bankReceived)

	if group, err := s.mihomoGroup(ctx, c); err == nil {
		st.Group = group
		if cur, err := c.Current(ctx, group); err == nil {
			st.Node = cur
			if native {
				st.EgressIP = all[cur].ExitIP
			}
		}
	}
	if native {
		writeJSON(w, http.StatusOK, st)
		return
	}
	if ip, err := c.EgressIP(ctx); err == nil {
		st.EgressIP = ip
	} else {
		// A dead exit looks the same whether the proxy is broken or the
		// subscription simply ran out. Distinguish them, because the fix is
		// completely different: one needs debugging, the other needs a renewal.
		if notice := quotaNotice(all); notice != "" {
			st.Error = "langganan proxy habis atau kedaluwarsa: " + notice
			st.QuotaExhausted = true
		} else if len(st.Groups) > 0 && st.totalNodes() == 0 {
			st.Error = "tidak ada node yang sehat di semua group; langganan kemungkinan habis"
			st.QuotaExhausted = true
		} else {
			st.Error = err.Error()
		}
	}

	writeJSON(w, http.StatusOK, st)
}

// totalNodes counts healthy nodes across every group.
func (st mihomoStatus) totalNodes() int {
	n := 0
	for _, g := range st.Groups {
		n += g.Nodes
	}
	return n
}

// quotaNotice looks for the account-notice rows a subscription injects when the
// plan is exhausted or expired.
//
// Providers replace the node list with text entries such as "剩余流量：0"
// (traffic remaining: 0) or "套餐到期：2026-09-21" (plan expires on …). Those
// entries are not servers, so every request through them fails. Detecting them
// turns a confusing "proxy unreachable" into an actionable message.
func quotaNotice(all map[string]mihomo.Group) string {
	for _, g := range all {
		if g.Type != "" && g.Type != "Selector" && g.Type != "URLTest" &&
			g.Type != "Fallback" && g.Type != "LoadBalance" {
			continue
		}
		for _, n := range g.All {
			if isNoticeName(n) {
				return n
			}
		}
	}
	// Also scan node names directly: a notice row is registered as its own
	// entry, so it appears as a top-level key.
	for name := range all {
		if isNoticeName(name) {
			return name
		}
	}
	return ""
}

// isNoticeName reports whether a node name is an account notice rather than a
// real server label.
func isNoticeName(name string) bool {
	for _, marker := range []string{
		"剩余流量", "距离下次重置", "套餐到期", "官网地址", "防失联",
		"流量重置", "到期时间", "过期时间", "续费", "购买",
	} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// handleMihomoSettings persists the connection settings.
func (s *Server) handleMihomoSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled    *bool  `json:"enabled"`
		Controller string `json:"controller"`
		Proxy      string `json:"proxy"`
		Secret     string `json:"secret"`
		Group      string `json:"group"`
		Rotate     *bool  `json:"rotate_on_limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}

	if body.Enabled != nil {
		v := "0"
		if *body.Enabled {
			v = "1"
		}
		_ = s.store.SetSetting(settingMihomoEnabled, v)
	}
	if body.Rotate != nil {
		v := "0"
		if *body.Rotate {
			v = "1"
		}
		_ = s.store.SetSetting(settingMihomoRotate, v)
	}
	for key, val := range map[string]string{
		settingMihomoController: body.Controller,
		settingMihomoProxy:      body.Proxy,
		settingMihomoSecret:     body.Secret,
		settingMihomoGroup:      body.Group,
	} {
		if strings.TrimSpace(val) != "" {
			_ = s.store.SetSetting(key, strings.TrimSpace(val))
		}
	}

	s.handleMihomoStatus(w, r)
}

// handleMihomoRotate switches to the next node.
func (s *Server) handleMihomoRotate(w http.ResponseWriter, r *http.Request) {
	// Rotation may try several nodes before finding a new egress IP, so this
	// needs a longer budget than a single switch.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	c := s.mihomoClient()
	group, err := s.mihomoGroup(ctx, c)
	if err != nil {
		writeAPIError(w, http.StatusPreconditionFailed, err.Error(), "mihomo_error")
		return
	}

	rotation, err := s.rotateMihomoNodeResult(ctx, c, group)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, mihomo.ErrSelectionChanged) || errors.Is(err, errNoAlternativeExit) {
			status = http.StatusConflict
		}
		writeAPIError(w, status, err.Error(), "mihomo_error")
		return
	}
	if nodeBankRuntimeEnabled() {
		ctx = mihomo.WithNode(ctx, rotation.Node)
	}
	ip, _ := c.EgressIP(ctx)
	ip = canonicalExitIP(ip)
	verified := rotation.PreviousIP != "" && ip != ""
	out := map[string]any{
		"ok":                 true,
		"group":              group,
		"from":               rotation.From,
		"node":               rotation.Node,
		"egress_ip":          ip,
		"previous_ip":        rotation.PreviousIP,
		"ip_changed":         verified && ip != rotation.PreviousIP,
		"ip_change_verified": verified,
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMihomoSelect switches to a specific node.
func (s *Server) handleMihomoSelect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Group string `json:"group"`
		Node  string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	if body.Node == "" {
		writeAPIError(w, http.StatusBadRequest, "node is required", "invalid_request_error")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	c := s.mihomoClient()
	group := body.Group
	if group == "" {
		var err error
		group, err = s.mihomoGroup(ctx, c)
		if err != nil {
			writeAPIError(w, http.StatusPreconditionFailed, err.Error(), "mihomo_error")
			return
		}
	}

	mihomoMu.Lock()
	err := c.Select(ctx, group, body.Node)
	mihomoMu.Unlock()
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error(), "mihomo_error")
		return
	}
	c.ResetConnections()

	time.Sleep(700 * time.Millisecond)
	ip, _ := c.EgressIP(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "group": group, "node": body.Node, "egress_ip": ip,
	})
}

// mihomoStreamZen sends a Zen request through mihomo and, on a per-IP rate limit
// or a region block, rotates to a node with a different egress IP and retries.
//
// This is the path that makes the integration useful: Zen limits the free tier
// per IP, so switching the exit node resets the limit. Retries are bounded
// because a subscription can have every node sharing one address.
func (s *Server) mihomoStreamZenOnce(
	ctx context.Context,
	model, sessionSeed string,
	body map[string]any,
	rec requestRecord,
) (<-chan zen.Event, int, error, string) {
	if !s.mihomoEnabled() {
		events, status, err, pool := s.streamZenWithRotation(ctx, model, sessionSeed, body, rec)
		return events, status, err, describeRelay(pool)
	}

	c := s.mihomoClient()
	group, err := s.mihomoGroup(ctx, c)
	if err != nil {
		events, status, streamErr, pool := s.streamZenWithRotation(ctx, model, sessionSeed, body, rec)
		return events, status, streamErr, describeRelay(pool)
	}

	client := s.zen.WithHTTP(c.ProxyClient())

	// Every attempt runs inside this loop, including the first.
	//
	// That matters: when the first attempt sat outside the loop, a single stalled
	// node consumed the entire retry budget and the loop then exited without ever
	// retrying. Measured as a request failing at exactly the 45s budget.
	//
	// Two bounds apply. maxAttempts caps the node switches, and retryBudget caps
	// total wall-clock time so a run of bad nodes cannot hold the client open.
	const maxAttempts = 8
	const retryBudget = 90 * time.Second

	// firstEventWait bounds the wait for the model to start producing output.
	//
	// Stream returns as soon as the response headers arrive, so a node that
	// answers 200 and then never emits an event would be reported as a success
	// and the client would simply hang. Measured before this guard: a request
	// that produced 17 tokens took 79 seconds. Waiting for the first event turns
	// that silent stall into an ordinary retryable failure.
	const firstEventWait = 12 * time.Second

	deadline := time.Now().Add(retryBudget)
	var lastErr error
	lastStatus := 0
	var attemptedNode string

	// Retrying and rotating are separate concerns, so the operator's switch
	// only controls the latter.
	//
	// Disabling rotation used to disable retrying too, which made a single
	// transient stall fail outright — measured as a 502 after a 22s header
	// timeout on a path with only one node. Retrying the same node absorbs that
	// kind of hiccup and needs no second node to exist.
	rotateEnabled := s.mihomoRotateOnLimit()

	for attempt := range maxAttempts {
		if attempt > 0 {
			if !shouldRetryZen(lastStatus, lastErr) {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			// A failure bound to the exit IP means this node is unusable for
			// now, whether or not we are about to rotate away from it. Marking
			// it lets the rotation skip a node that mihomo still calls healthy
			// but Zen rejects.
			ipTied := ipTiedFailure(lastStatus, lastErr.Error())
			if ipTied {
				s.penalizeNode(group, attemptedNode)
			}
			if rotateEnabled {
				if _, _, rotErr := s.rotateMihomoNode(ctx, c, group, attemptedNode); rotErr != nil {
					// A concurrent switch already chose a replacement, or only
					// this exit remains. Retry the currently selected node within
					// the same bounded budget; never overwrite a stale selection.
					if !errors.Is(rotErr, mihomo.ErrSelectionChanged) && !errors.Is(rotErr, errNoAlternativeExit) {
						break
					}
				}
			} else if ipTied {
				// Without rotation there is no new exit to try, and this failure
				// follows the exit IP. Repeating it against the same node would
				// only spend the client's time on a known-losing request.
				break
			}
		}

		// The context must not carry a deadline: it stays open for the life of
		// the stream, and a deadline here would truncate a long but healthy
		// generation. Stalls are caught by the header timeout and by
		// firstEventWait instead, neither of which bounds the whole response.
		attemptCtx, cancel := context.WithCancel(ctx)
		attemptedNode, _ = c.Current(ctx, group)
		if nodeBankRuntimeEnabled() && attemptedNode != "" {
			attemptCtx = mihomo.WithNode(attemptCtx, attemptedNode)
		}
		events, status, err := client.Stream(attemptCtx, model, sessionSeed, body)
		lastStatus = status
		if err == nil {
			first, rest, waitErr := streamFirst(attemptCtx, events, firstEventWait)
			if waitErr == nil {
				// A success does NOT clear an earlier rejection.
				//
				// Zen's limit is a budget over a multi-hour window, not a
				// momentary refusal: an IP can answer one request and still be
				// nearly exhausted. Forgetting the penalty here let rotation
				// return to that IP, and the next burst hit the wall again.
				// The penalty therefore expires only by time.
				//
				// Hand back a channel that replays the first event, so waiting
				// for it here loses nothing for the caller. The context stays
				// open for the life of the stream and is released with it.
				return detachCancel(attemptCtx, replayFirst(attemptCtx, first, rest), cancel), status, nil, "mihomo:" + group
			}
			err = waitErr
		}
		cancel()
		lastErr = err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("zen: tidak ada percobaan yang berhasil")
	}
	if lastStatus < 400 {
		lastStatus = http.StatusBadGateway
	}
	return nil, lastStatus, lastErr, "mihomo:" + group
}

// streamFirst waits for a stream's first event and reports a stall when none
// arrives within d.
//
// The returned channel replays that first event followed by the rest, so the
// caller sees the complete stream. This exists because an open HTTP response is
// not evidence that the upstream is actually working.
func streamFirst(ctx context.Context, events <-chan zen.Event, d time.Duration) (zen.Event, <-chan zen.Event, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case ev, ok := <-events:
		if !ok {
			return zen.Event{}, nil, &initialStreamError{detail: "zen: stream ditutup sebelum ada event"}
		}
		if ev.Error != nil {
			return zen.Event{}, nil, ev.Error
		}
		return ev, events, nil
	case <-timer.C:
		return zen.Event{}, nil, &initialStreamError{detail: fmt.Sprintf("zen: stream diam tanpa event selama %s", d)}
	case <-ctx.Done():
		return zen.Event{}, nil, ctx.Err()
	}
}

// detachCancel releases cancel once the caller has finished reading out.
//
// The attempt context must stay alive for as long as the stream is being read,
// but it must not leak afterwards: cancelling it at close time releases the
// underlying connection without truncating the response.
func detachCancel(ctx context.Context, in <-chan zen.Event, cancel context.CancelFunc) <-chan zen.Event {
	out := make(chan zen.Event, 16)
	go func() {
		defer close(out)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// replayFirst re-emits first and then drains rest, so a stream can be inspected
// for its first event without the caller losing it.
func replayFirst(ctx context.Context, first zen.Event, rest <-chan zen.Event) <-chan zen.Event {
	out := make(chan zen.Event, 16)
	go func() {
		defer close(out)
		select {
		case out <- first:
		case <-ctx.Done():
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-rest:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
