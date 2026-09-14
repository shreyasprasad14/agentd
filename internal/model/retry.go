package model

import "errors"

// NonRetryableError wraps a provider error that cannot succeed on a retry:
// a refusal, an unknown or unpriced model, a bad request, or an
// authentication failure. modelStep fails the run on the first one instead
// of spending its retry budget on an outcome that cannot change.
//
// This is the follow-up ADR-12 promised. The loop retries every provider
// error three times because it cannot tell a dropped connection from a
// refusal, and for a local runtime still loading a model that default is
// right. For an invalid API key it is three round trips and two backoffs of
// pure latency in front of a failure that was already decided, and the run's
// recorded error is the third attempt's rather than the first's. The marker
// travels on the error itself rather than in a predicate the loop owns so
// that the provider — the only code that knows what the status code meant —
// is the one that decides (ADR-27).
type NonRetryableError struct{ Err error }

// Error reports the wrapped error verbatim. The marker changes how the loop
// treats the error, not what it says: run_finished.error should read the same
// whether or not the failure was worth another attempt.
func (e *NonRetryableError) Error() string {
	if e == nil || e.Err == nil {
		return "non-retryable model error"
	}
	return e.Err.Error()
}

// Unwrap exposes the underlying error, so errors.Is and errors.As still reach
// a provider's own typed errors through the marker.
func (e *NonRetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NonRetryable marks err as not worth retrying. It returns nil for nil, so it
// can be applied to a result without first testing it.
func NonRetryable(err error) error {
	if err == nil {
		return nil
	}
	return &NonRetryableError{Err: err}
}

// IsNonRetryable reports whether err was marked by NonRetryable. It looks
// through wrapping, so a caller that adds its own context to a provider error
// does not erase the marker.
func IsNonRetryable(err error) bool {
	var nr *NonRetryableError
	return errors.As(err, &nr)
}
