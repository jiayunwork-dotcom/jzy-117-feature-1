package runner

import (
	"crypto/rand"
	"encoding/hex"
	"math"

	"openchannel/internal/profile"
)

// newID returns a 128-bit random hex identifier.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// compareSampleSpacing is the common-grid spacing for new-vs-old summaries.
const compareSampleSpacing = 20.0

// Compare profiles of two versions on a common xi grid over their shared
// length (xi measured from the downstream control). Both pushes start at the
// same control, so interpolated depths line up physically.
func Compare(old, new *profile.Profile, fromVersion, toVersion int) *Comparison {
	xMax := math.Min(old.TotalLength, new.TotalLength)
	if xMax < compareSampleSpacing {
		xMax = math.Max(old.TotalLength, new.TotalLength)
	}
	c := &Comparison{FromVersion: fromVersion, ToVersion: toVersion}

	n := int(math.Ceil(xMax/compareSampleSpacing)) + 1
	sum := 0.0
	for i := 0; i < n; i++ {
		xi := math.Min(float64(i)*compareSampleSpacing, xMax)
		d := new.DepthAt(xi) - old.DepthAt(xi)
		sum += math.Abs(d)
		if d > c.MaxDepthIncrease {
			c.MaxDepthIncrease = d
		}
		if d < c.MaxDepthDecrease {
			c.MaxDepthDecrease = d
		}
		if math.Abs(d) > c.MaxAbsDelta {
			c.MaxAbsDelta = math.Abs(d)
			c.MaxAbsDeltaAt = xi
		}
	}
	c.MeanAbsDelta = sum / float64(n)

	// If both lines carry the same downstream-reach layout (same length),
	// report the maximum delta restricted to that reach, which must stay
	// unchanged when only an upstream reach was edited.
	if len(old.Reaches) == len(new.Reaches) {
		downOld := old.Reaches[len(old.Reaches)-1]
		downNew := new.Reaches[len(new.Reaches)-1]
		if downOld.Length == downNew.Length {
			for xi := 0.0; xi <= downOld.Length; xi += compareSampleSpacing {
				if dd := math.Abs(new.DepthAt(xi) - old.DepthAt(xi)); dd > c.DownstreamReachMaxDelta {
					c.DownstreamReachMaxDelta = dd
				}
			}
		}
	}
	return c
}
