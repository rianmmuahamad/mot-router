package zen

import (
	"context"
	"reflect"
)

type requestProfileKey struct{}
type requestProfile struct{ profile *GateProfile }

func SameGateProfile(a, b *GateProfile) bool {
	return reflect.DeepEqual(cloneGateProfile(a), cloneGateProfile(b))
}

// WithGateSnapshot binds body translation and outbound transport to one profile.
// A nil snapshot is meaningful: do not consult a newly refreshed provider later.
func WithGateSnapshot(ctx context.Context, profile *GateProfile) context.Context {
	return context.WithValue(ctx, requestProfileKey{}, requestProfile{cloneGateProfile(profile)})
}

func GateSnapshot(ctx context.Context, fallback func() *GateProfile) *GateProfile {
	if snapshot, ok := ctx.Value(requestProfileKey{}).(requestProfile); ok {
		return snapshot.profile
	}
	return cloneGateProfile(fallback())
}

func cloneGateProfile(profile *GateProfile) *GateProfile {
	if profile == nil {
		return nil
	}
	copy := *profile
	copy.RequiredTools = append([]string(nil), profile.RequiredTools...)
	copy.ToolDescriptions = make(map[string]string, len(profile.ToolDescriptions))
	for k, v := range profile.ToolDescriptions {
		copy.ToolDescriptions[k] = v
	}
	return &copy
}

func (c *Client) requestClient(ctx context.Context) *Client {
	copy := *c
	copy.Gate = GateSnapshot(ctx, c.CurrentGate)
	copy.GateProvider = nil
	return &copy
}
