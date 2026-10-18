package tools

import (
	"errors"
	"fmt"
	"time"

	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// retryableError is a Tool error after which the Caller may make exactly
// the same call again, once wait has passed. The call changed nothing.
// Its text starts with "retryable: ", which is how the Caller tells it
// apart: MCP gives Tool errors no structured form.
type retryableError struct {
	reason string // what happened, in storewarden's words
	wait   time.Duration
}

func (e *retryableError) Error() string {
	return fmt.Sprintf("retryable: %s. Retry the same call in %s", e.reason, e.wait)
}

// toolError returns the error the Caller sees for err, which a Tool's
// handler returned. The Caller's spent Quota and Shopify's rate limit
// become retryable errors with the suggested wait, each worded so the
// Caller can tell which it was. Any other error is returned as it is.
func toolError(err error) error {
	var spent *shopify.QuotaError
	if errors.As(err, &spent) {
		return &retryableError{
			reason: "your Quota of the Store's rate limit is used up, so nothing was done",
			wait:   spent.Wait,
		}
	}
	var throttled *shopify.ThrottledError
	if errors.As(err, &throttled) {
		return &retryableError{
			reason: "Shopify's rate limit for the Store can't serve this call before its deadline, so nothing was done",
			wait:   throttled.Wait,
		}
	}
	return err
}
