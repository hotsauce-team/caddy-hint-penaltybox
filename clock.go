package penaltybox

import "time"

// clock abstracts time.Now so store logic is testable without sleeps.
// time.Time values from realClock carry Go's monotonic reading, making
// all window/box arithmetic immune to wall-clock jumps.
type clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
