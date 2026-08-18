package conn

import "context"

// sourcePodCtxKey carries the source pod name through the capture context.
// Reentrancy seam: the enterprise DaemonSet agent sets it per connection
// (WithSourcePod) so Capture can stamp TestCase.SourcePod for per-pod
// attribution. OSS single-app callers never set it, so SourcePod stays empty.
type sourcePodCtxKey struct{}

// WithSourcePod returns a context carrying the source pod name. A no-op-shaped
// empty pod is allowed (stamps an empty SourcePod, same as not setting it).
func WithSourcePod(ctx context.Context, pod string) context.Context {
	return context.WithValue(ctx, sourcePodCtxKey{}, pod)
}

// SourcePodFromContext returns the source pod name carried on ctx, or "" when
// none was set.
func SourcePodFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if pod, ok := ctx.Value(sourcePodCtxKey{}).(string); ok {
		return pod
	}
	return ""
}
