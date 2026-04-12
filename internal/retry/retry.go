// Package retry provides a small generic exponential-backoff helper for
// wrapping Gemini and Google API calls that may fail transiently.
package retry

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
)

type Options struct {
	Attempts  int
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

func defaultOpts() Options {
	return Options{Attempts: 4, BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second}
}

func Do[T any](ctx context.Context, fn func(context.Context) (T, error), opts ...Options) (T, error) {
	o := defaultOpts()
	if len(opts) > 0 {
		o = opts[0]
	}
	var zero T
	var lastErr error
	for attempt := 0; attempt < o.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		v, err := fn(ctx)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if !isRetriable(err) {
			return zero, err
		}
		// Exponential backoff: base * 2^attempt, capped, jittered ±20%.
		delay := o.BaseDelay << attempt
		if delay > o.MaxDelay {
			delay = o.MaxDelay
		}
		jitter := time.Duration(rand.Int63n(int64(delay / 5)))
		if rand.Intn(2) == 0 {
			delay -= jitter
		} else {
			delay += jitter
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	return zero, lastErr
}

func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	var gapi *googleapi.Error
	if errors.As(err, &gapi) {
		switch gapi.Code {
		case 408, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	// Gemini SDK currently returns errors whose strings include the RPC status.
	// Match on common transient tokens until typed errors ship.
	msg := strings.ToUpper(err.Error())
	for _, needle := range []string{"RESOURCE_EXHAUSTED", "UNAVAILABLE", "DEADLINE_EXCEEDED", "INTERNAL", "ABORTED", " 429", " 500", " 502", " 503", " 504"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
