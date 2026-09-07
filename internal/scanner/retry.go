package scanner

import "context"

// attemptKey carries the attempt a scan is on.
type attemptKey struct{}

// attemptInfo is what a caller tells the scanner about the attempt it is
// making, so the stored result can explain itself (Story 3.8, AC10).
type attemptInfo struct {
	attempt  int
	attempts int
	previous string
}

// WithAttempt labels a scan as one attempt of several.
//
// It travels in the context for the same reason the source does: Scan's
// signature is the seam the scheduler implements, and widening it for
// bookkeeping the scanner only records would put the retry policy into every
// implementation of that seam.
func WithAttempt(ctx context.Context, attempt, attempts int, previousError string) context.Context {
	if attempt < 1 {
		attempt = 1
	}

	return context.WithValue(ctx, attemptKey{}, attemptInfo{
		attempt:  attempt,
		attempts: attempts,
		previous: previousError,
	})
}

func attemptOf(ctx context.Context) attemptInfo {
	if info, ok := ctx.Value(attemptKey{}).(attemptInfo); ok {
		return info
	}

	return attemptInfo{attempt: 1, attempts: 1}
}
