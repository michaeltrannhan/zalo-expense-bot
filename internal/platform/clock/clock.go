// Package clock abstracts time so flows can be tested deterministically.
package clock

import "time"

// Clock supplies the current time.
type Clock interface {
	Now() time.Time
}

// Real is the production clock (UTC).
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

// Fixed is a test clock returning a constant instant.
type Fixed struct{ T time.Time }

func (f Fixed) Now() time.Time { return f.T }
