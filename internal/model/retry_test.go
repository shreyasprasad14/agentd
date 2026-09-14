package model

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// sentinel stands in for a provider's own typed error, which must stay
// reachable through the marker.
var errSentinel = errors.New("refused")

func TestNonRetryable_WrapsAndUnwraps(t *testing.T) {
	marked := NonRetryable(errSentinel)
	require.True(t, IsNonRetryable(marked))
	require.ErrorIs(t, marked, errSentinel, "errors.Is reaches through the marker")
	require.Equal(t, "refused", marked.Error(), "the message is the provider's, unchanged")
	require.Equal(t, errSentinel, errors.Unwrap(marked))

	// A caller that adds its own context does not erase the marker.
	wrapped := fmt.Errorf("model call: %w", marked)
	require.True(t, IsNonRetryable(wrapped))
	require.ErrorIs(t, wrapped, errSentinel)

	var nr *NonRetryableError
	require.ErrorAs(t, wrapped, &nr)
	require.Equal(t, errSentinel, nr.Err)
}

func TestNonRetryable_NilAndUnmarked(t *testing.T) {
	require.NoError(t, NonRetryable(nil), "nil in, nil out, so it can wrap a result directly")
	require.False(t, IsNonRetryable(nil))
	require.False(t, IsNonRetryable(errSentinel), "an ordinary error is retryable")
	require.False(t, IsNonRetryable(fmt.Errorf("wrapped: %w", errSentinel)))

	// A zero value is degenerate but must not panic: Error is called on
	// whatever reaches run_finished.error.
	require.NotPanics(t, func() {
		require.Equal(t, "non-retryable model error", (&NonRetryableError{}).Error())
		var typed *NonRetryableError
		require.Equal(t, "non-retryable model error", typed.Error())
		require.NoError(t, typed.Unwrap())
	})
}
