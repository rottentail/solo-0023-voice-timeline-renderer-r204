package render

import (
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

// normalizationGain computes the peak-normalization gain exactly as the
// historical in-place normalizer did: scale the true peak to the requested
// target, falling back to unity for silent or degenerate mixes. The gain is
// applied sample-by-sample during output encoding.
func normalizationGain(peak float64, targetDBFS float64) float64 {
	gain := 1.0
	if peak > 0 {
		gain = audiocodec.DBToLinear(targetDBFS) / peak
	}
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain <= 0 {
		gain = 1
	}
	return gain
}

// applyGain mirrors the historical normalizer's per-sample arithmetic:
// float64 scaling, hard clamp to full scale, then float32 storage.
func applyGain(sample float32, gain float64) float32 {
	value := float64(sample) * gain
	if value > 1 {
		value = 1
	}
	if value < -1 {
		value = -1
	}
	return float32(value)
}
