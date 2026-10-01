package profile

import (
	"openchannel/internal/channel"
	"openchannel/internal/flow"
	"openchannel/internal/manning"
)

func normalDepthOf(def channel.Definition, reach int) (float64, error) {
	r := def.Reaches[reach]
	return manning.NormalDepth(manning.Input{
		Section:   r.Section,
		Roughness: r.Roughness,
		Slope:     r.Slope,
		Flow:      def.DesignFlow,
	})
}

func criticalDepthOf(def channel.Definition, reach int) float64 {
	return flow.CriticalDepth(def.Reaches[reach].Section, def.DesignFlow)
}
