package disclosures

import (
	"errors"
	"fmt"
	"net"
)

// permanentDisclosureFailure marks invalid upstream data and policy failures
// which an automatic retry cannot correct.
type permanentDisclosureFailure struct{ cause error }

func (e *permanentDisclosureFailure) Error() string { return e.cause.Error() }
func (e *permanentDisclosureFailure) Unwrap() error { return e.cause }

func permanentDisclosureError(err error) error {
	if err == nil {
		return nil
	}
	return &permanentDisclosureFailure{cause: err}
}

type disclosureHTTPError struct {
	Source     string
	StatusCode int
	Status     string
}

func (e *disclosureHTTPError) Error() string {
	return fmt.Sprintf("%s disclosure document: HTTP %s", e.Source, e.Status)
}

func disclosureHTTPStatusError(source string, statusCode int, status string) error {
	return &disclosureHTTPError{Source: source, StatusCode: statusCode, Status: status}
}

func shouldRetryDisclosureFailure(err error) bool {
	var permanent *permanentDisclosureFailure
	if errors.As(err, &permanent) {
		return false
	}
	var httpErr *disclosureHTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 408 || httpErr.StatusCode == 429 || httpErr.StatusCode >= 500
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	// PDF parsing and response decoding can be transient (for example after a
	// provider rollout); the durable retry budget bounds their impact.
	return true
}
