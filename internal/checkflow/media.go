package checkflow

import (
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

func sineWAV(sampleRate, channels, durationMS int, frequency, amplitude float64) ([]byte, error) {
	frames := sampleRate * durationMS / 1000
	samples := make([]float32, frames*channels)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(sampleRate)
		first := float32(math.Sin(2*math.Pi*frequency*seconds) * amplitude)
		if channels == 1 {
			samples[frame] = first
			continue
		}
		second := float32(math.Sin(2*math.Pi*frequency*seconds+0.35) * amplitude * 0.8)
		samples[frame*2] = first
		samples[frame*2+1] = second
	}
	return audiocodec.EncodePCM16(sampleRate, channels, samples)
}

// xorshift64star is a small deterministic PRNG so the check can synthesize
// independent noise signals without any external fixture.
type xorshift64star uint64

func (state *xorshift64star) next() uint64 {
	value := uint64(*state)
	value ^= value >> 12
	value ^= value << 25
	value ^= value >> 27
	*state = xorshift64star(value)
	return value * 2685821657736338717
}

// uniform returns the next variate in [-1, 1).
func (state *xorshift64star) uniform() float64 {
	return float64(int64(state.next()>>11))*(1.0/mathPow2_52) - 1.0
}

const mathPow2_52 = 4503599627370496.0

// noiseSamples synthesizes frames of independent mono noise at the requested
// amplitude. Distinct seeds produce statistically independent signals, which
// is what makes the uncorrelated-seam scenario independent of the renderer.
func noiseSamples(frames int, seed uint64, amplitude float64) []float32 {
	if seed == 0 {
		seed = 1
	}
	state := xorshift64star(seed)
	samples := make([]float32, frames)
	for index := range samples {
		samples[index] = float32(state.uniform() * amplitude)
	}
	return samples
}

// toneSamples synthesizes a mono sine.
func toneSamples(frames int, sampleRate int, frequency, amplitude float64) []float32 {
	samples := make([]float32, frames)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(sampleRate)
		samples[frame] = float32(math.Sin(2*math.Pi*frequency*seconds) * amplitude)
	}
	return samples
}

// mixtureSamples synthesizes a sum of sines, giving the correlated scenario a
// shared segment with more than one spectral component.
func mixtureSamples(frames int, sampleRate int, components [][2]float64) []float32 {
	samples := make([]float32, frames)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(sampleRate)
		value := 0.0
		for _, component := range components {
			value += math.Sin(2*math.Pi*component[0]*seconds) * component[1]
		}
		samples[frame] = float32(value)
	}
	return samples
}

func concatSamples(parts ...[]float32) []float32 {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	combined := make([]float32, 0, total)
	for _, part := range parts {
		combined = append(combined, part...)
	}
	return combined
}

func negateSamples(samples []float32) []float32 {
	negated := make([]float32, len(samples))
	for index, sample := range samples {
		negated[index] = -sample
	}
	return negated
}

func stereoSamples(left []float32, right []float32) []float32 {
	frames := len(left)
	samples := make([]float32, frames*2)
	for frame := 0; frame < frames; frame++ {
		samples[frame*2] = left[frame]
		samples[frame*2+1] = right[frame]
	}
	return samples
}

func encodeMonoWAV(sampleRate int, samples []float32) ([]byte, error) {
	return audiocodec.EncodePCM16(sampleRate, 1, samples)
}

func encodeStereoWAV(sampleRate int, samples []float32) ([]byte, error) {
	return audiocodec.EncodePCM16(sampleRate, 2, samples)
}

// rmsDBFS measures the RMS level of one interleaved channel range.
func rmsDBFS(samples []float32, fromFrame, toFrame, channels int) float64 {
	if toFrame <= fromFrame || channels <= 0 {
		return math.NaN()
	}
	sumSquares := 0.0
	count := 0
	for frame := fromFrame; frame < toFrame; frame++ {
		for channel := 0; channel < channels; channel++ {
			value := float64(samples[frame*channels+channel])
			sumSquares += value * value
			count++
		}
	}
	if count == 0 || sumSquares == 0 {
		return -120
	}
	return 20 * math.Log10(math.Sqrt(sumSquares/float64(count)))
}

// pearsonMono correlates two equal-length mono sample slices. The check uses
// it to verify the renderer preserved polarity in solo regions.
func pearsonMono(left, right []float32) float64 {
	if len(left) != len(right) || len(left) < 2 {
		return 0
	}
	n := float64(len(left))
	var sumL, sumR, sumLL, sumRR, sumLR float64
	for index := range left {
		l := float64(left[index])
		r := float64(right[index])
		sumL += l
		sumR += r
		sumLL += l * l
		sumRR += r * r
		sumLR += l * r
	}
	numerator := n*sumLR - sumL*sumR
	denominator := math.Sqrt((n*sumLL - sumL*sumL) * (n*sumRR - sumR*sumR))
	if denominator == 0 {
		return 0
	}
	return numerator / denominator
}
