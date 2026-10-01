// Package validation centralises input checks so that every entry point
// (Manning solver, weir formula, HTTP layer) rejects bad numbers before any
// hydraulic computation runs. NaN is rejected by every check because no
// comparison against it is true.
package validation

import "fmt"

// Error names the offending field and explains the constraint.
type Error struct {
	Field   string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func fail(field, message string) error {
	return &Error{Field: field, Message: message}
}

// Positive requires v > 0 (used for roughness, slope, bottom width, weir
// width and weir head).
func Positive(field string, v float64) error {
	if !(v > 0) {
		return fail(field, "must be greater than 0")
	}
	return nil
}

// NonNegative requires v >= 0 (used for flow rate and side slope).
func NonNegative(field string, v float64) error {
	if !(v >= 0) {
		return fail(field, "must be greater than or equal to 0")
	}
	return nil
}
