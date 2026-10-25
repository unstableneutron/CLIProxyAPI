package auth

import "context"

type codexClientRequestSnapshotKey struct{}

// withCodexClientRequestSnapshot records the gin request's Codex-client
// classification while the handler still owns the gin.Context. Goroutines that
// outlive the handler, such as stream result recording, then read the snapshot
// instead of the pooled gin.Context, which gin reuses for later requests.
func withCodexClientRequestSnapshot(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if _, ok := codexClientRequestSnapshot(ctx); ok {
		return ctx
	}
	return context.WithValue(ctx, codexClientRequestSnapshotKey{}, IsCodexClientRequest(ctx))
}

func codexClientRequestSnapshot(ctx context.Context) (bool, bool) {
	snapshot, ok := ctx.Value(codexClientRequestSnapshotKey{}).(bool)
	return snapshot, ok
}
