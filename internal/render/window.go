package render

import (
	"math"

	"voice-timeline-renderer/internal/domain"
)

// windowFactory produces bounded windows of span output frames on demand.
// Each window applies the exact slice -> resample -> channel-map chain the
// historical whole-clip pipeline used, computed per output frame so windows
// are independent and never require retaining a span's decoded audio. The
// factory owns its scratch buffers and reuses them across calls; it is not
// safe for concurrent use.
type windowFactory struct {
	timelineRate   int
	outputChannels int
	sourceScratch  []float32
	window         []float32
}

func newWindowFactory(plan domain.Timeline) *windowFactory {
	return &windowFactory{
		timelineRate:   plan.SampleRate,
		outputChannels: plan.OutputChannels,
	}
}

// windowFor returns a scratch window buffer sized for frames output frames.
func (factory *windowFactory) windowFor(frames int64) []float32 {
	need := frames * int64(factory.outputChannels)
	if int64(cap(factory.window)) < need {
		factory.window = make([]float32, need)
	}
	return factory.window[:need]
}

// produce fills dst[0:frames*outputChannels] with the span's output frames
// [startLocal, startLocal+frames). dst must be pre-sized by the caller.
func (factory *windowFactory) produce(
	handle *sourceHandle,
	span plannedSpan,
	startLocal int64,
	frames int64,
	dst []float32,
) error {
	info := handle.frames.Info()
	sourceChannels := info.Channels
	sliceFrames := span.SourceEndFrame - span.SourceStartFrame
	step := float64(info.SampleRate) / float64(factory.timelineRate)

	// Source frames touched by this window: the floor of the first and last
	// output positions, plus one frame of interpolation headroom, clamped to
	// the span's source interval exactly like the historical resampler.
	lo := int64(float64(startLocal) * step)
	hi := int64(float64(startLocal+frames-1)*step) + 1
	if lo >= sliceFrames {
		lo = sliceFrames - 1
	}
	if hi >= sliceFrames {
		hi = sliceFrames - 1
	}
	need := (hi - lo + 1) * int64(sourceChannels)
	if int64(cap(factory.sourceScratch)) < need {
		factory.sourceScratch = make([]float32, need)
	}
	source := factory.sourceScratch[:need]
	if err := handle.frames.ReadFrames(span.SourceStartFrame+lo, source); err != nil {
		return err
	}

	outChannels := int64(factory.outputChannels)
	for k := int64(0); k < frames; k++ {
		position := float64(startLocal+k) * step
		left := int64(position)
		if left >= sliceFrames-1 {
			left = sliceFrames - 1
		}
		right := left + 1
		if right >= sliceFrames {
			right = sliceFrames - 1
		}
		weight := float32(position - float64(left))
		leftOffset := (left - lo) * int64(sourceChannels)
		rightOffset := (right - lo) * int64(sourceChannels)
		switch {
		case sourceChannels == factory.outputChannels:
			for channel := int64(0); channel < outChannels; channel++ {
				first := source[leftOffset+channel]
				second := source[rightOffset+channel]
				dst[k*outChannels+channel] = first + (second-first)*weight
			}
		case sourceChannels == 1:
			first := source[leftOffset]
			second := source[rightOffset]
			value := first + (second-first)*weight
			dst[k*outChannels] = value
			dst[k*outChannels+1] = value
		default:
			firstLeft := source[leftOffset]
			secondLeft := source[rightOffset]
			firstRight := source[leftOffset+1]
			secondRight := source[rightOffset+1]
			leftValue := firstLeft + (secondLeft-firstLeft)*weight
			rightValue := firstRight + (secondRight-firstRight)*weight
			dst[k*outChannels] = (leftValue + rightValue) * 0.5
		}
	}
	return nil
}

// fadeIn evaluates the incoming side of the joint law: sin^γ(u·π/2). γ = 1 is
// the equal-power law; γ = 2 is the constant-amplitude law.
func fadeIn(u float64, exponent float64) float32 {
	return float32(math.Pow(math.Sin(u*math.Pi/2), exponent))
}

// fadeOut evaluates the outgoing side of the joint law: cos^γ(u·π/2).
func fadeOut(u float64, exponent float64) float32 {
	return float32(math.Pow(math.Cos(u*math.Pi/2), exponent))
}
