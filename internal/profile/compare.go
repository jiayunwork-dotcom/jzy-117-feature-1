package profile

import "math"

// Comparison is a new-vs-old profile summary attached to a recomputed result.
type Comparison struct {
	BaseVersion int `json:"base_version"`
	// Point of largest absolute depth change, in metres upstream of control.
	MaxDeltaDistance float64 `json:"max_delta_distance"`
	MaxDeltaDepth    float64 `json:"max_delta_depth"` // signed: new - old, metres
	// Extent actually compared: distance 0..ComparedLength.
	ComparedLength float64 `json:"compared_length_metres"`
	MeanAbsDelta   float64 `json:"mean_abs_delta_depth"` // metres, over sampled stations
	SampleCount    int     `json:"sample_count"`
}

// CompareProfiles compares a new profile against the profile stored for
// baseVersion. Stations of both profiles are sampled on the finer of the two
// grids; depth is obtained by linear interpolation (DepthAt). Velocities are
// not compared numerically - the response already carries both profiles when
// the caller needs them.
func CompareProfiles(baseVersion int, oldProf, newProf *Profile) *Comparison {
	limit := math.Min(oldProf.TotalLength, newProf.TotalLength)

	// Collect the union of sample distances within the common extent.
	seen := map[float64]bool{0: true, limit: true}
	samples := []float64{0}
	add := func(s float64) {
		if s < 0 || s > limit+1e-9 {
			return
		}
		key := math.Round(s*1e6) / 1e6
		if !seen[key] {
			seen[key] = true
			samples = append(samples, s)
		}
	}
	for _, pt := range oldProf.Points {
		add(pt.Distance)
	}
	for _, pt := range newProf.Points {
		add(pt.Distance)
	}
	// Sort ascending (small sets; avoid importing sort for clarity is not
	// worth it - use sort).
	sortFloat64s(samples)

	c := &Comparison{BaseVersion: baseVersion, ComparedLength: limit}
	var sum float64
	for _, s := range samples {
		d := DepthAt(newProf, s) - DepthAt(oldProf, s)
		sum += math.Abs(d)
		if math.Abs(d) > math.Abs(c.MaxDeltaDepth) {
			c.MaxDeltaDepth = d
			c.MaxDeltaDistance = s
		}
	}
	c.SampleCount = len(samples)
	if len(samples) > 0 {
		c.MeanAbsDelta = sum / float64(len(samples))
	}
	return c
}

func sortFloat64s(xs []float64) {
	// Insertion sort: sample counts are at most a few hundred.
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
