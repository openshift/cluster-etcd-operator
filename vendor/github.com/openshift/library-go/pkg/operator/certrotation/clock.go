package certrotation

import (
	"k8s.io/utils/clock"
)

// certrotationClock returns the given clock, or the real system clock when nil.
// All Clock fields in this package are optional: leaving them unset preserves
// the historical behavior of reading the system clock.
func certrotationClock(c clock.PassiveClock) clock.PassiveClock {
	if c == nil {
		return clock.RealClock{}
	}
	return c
}
