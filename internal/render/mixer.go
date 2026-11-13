package render

import (
	"context"
	"errors"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// mixBlockFrames bounds the working set while mixing. The mix is streamed to
// its consumer one output block at a time, so memory stays bounded no matter
// how long the timeline is: there is no full-length mix buffer anywhere in
// the pipeline.
const mixBlockFrames = 32768

// forEachMixBlock streams the mixed timeline in output-frame order, invoking
// consume for each bounded block. Every span only ever contributes inside
// [OutputStartFrame, OutputEndFrame), so each source's audible range matches
// the placement recorded in the receipt exactly — including when a short
// middle span's incoming and outgoing fades overlap, where the two envelopes
// simply multiply. The same iterator drives the peak pass and the encode
// pass, so both observe identical samples.
func forEachMixBlock(
	ctx context.Context,
	plan domain.Timeline,
	spans []plannedSpan,
	joints []jointDecision,
	cache *sourceCache,
	totalFrames int64,
	consume func(block []float32) error,
) error {
	channels := plan.OutputChannels
	if totalFrames < 1 || channels < 1 {
		return errors.New("mix dimensions are invalid")
	}
	if totalFrames > int64(^uint(0)>>1)/int64(channels) {
		return errors.New("mix is too large for this process")
	}
	factory := newWindowFactory(plan)
	blockBuf := make([]float32, mixBlockFrames*channels)
	spanIndex := 0
	for blockStart := int64(0); blockStart < totalFrames; blockStart += mixBlockFrames {
		if err := ctx.Err(); err != nil {
			return err
		}
		blockEnd := blockStart + mixBlockFrames
		if blockEnd > totalFrames {
			blockEnd = totalFrames
		}
		blockFrames := blockEnd - blockStart
		clear(blockBuf)
		// Spans are ordered by output start; advance past spans that ended
		// before this block, then accumulate every span overlapping it.
		for spanIndex < len(spans) && spans[spanIndex].OutputEndFrame <= blockStart {
			spanIndex++
		}
		for index := spanIndex; index < len(spans) && spans[index].OutputStartFrame < blockEnd; index++ {
			span := spans[index]
			if err := accumulateSpan(ctx, factory, cache, plan, span, joints, blockBuf, blockStart, blockEnd); err != nil {
				return err
			}
		}
		if err := consume(blockBuf[:blockFrames*int64(channels)]); err != nil {
			return err
		}
	}
	return nil
}

// accumulateSpan adds one span's contribution to the output block
// [blockStart, blockEnd) under its gain and fade envelopes.
func accumulateSpan(
	ctx context.Context,
	factory *windowFactory,
	cache *sourceCache,
	plan domain.Timeline,
	span plannedSpan,
	joints []jointDecision,
	blockBuf []float32,
	blockStart int64,
	blockEnd int64,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	channels := int64(plan.OutputChannels)
	localStart := blockStart - span.OutputStartFrame
	if localStart < 0 {
		localStart = 0
	}
	localEnd := blockEnd - span.OutputStartFrame
	if localEnd > span.OutputFrames {
		localEnd = span.OutputFrames
	}
	if localEnd <= localStart {
		return nil
	}
	handle, err := cache.get(span.SourceID)
	if err != nil {
		return err
	}
	frames := localEnd - localStart
	window := factory.windowFor(frames)
	if err := factory.produce(handle, span, localStart, frames, window); err != nil {
		return err
	}
	gain := float32(audiocodec.DBToLinear(span.GainDB))
	inFade := span.CrossfadeFrames
	inExponent := joints[span.Index].Exponent
	outFade := int64(0)
	outExponent := 1.0
	if span.Index+1 < len(joints) {
		outFade = joints[span.Index+1].Frames
		outExponent = joints[span.Index+1].Exponent
	}
	for k := int64(0); k < frames; k++ {
		local := localStart + k
		weight := gain
		if inFade > 0 && local < inFade {
			u := (float64(local) + 0.5) / float64(inFade)
			weight *= fadeIn(u, inExponent)
		}
		if outFade > 0 && local >= span.OutputFrames-outFade {
			u := (float64(local-(span.OutputFrames-outFade)) + 0.5) / float64(outFade)
			weight *= fadeOut(u, outExponent)
		}
		target := (localStart + k + span.OutputStartFrame - blockStart) * channels
		source := k * channels
		for channel := int64(0); channel < channels; channel++ {
			blockBuf[target+channel] += window[source+channel] * weight
		}
	}
	return nil
}

// mixPeak streams the mix once and returns its true peak amplitude.
func mixPeak(
	ctx context.Context,
	plan domain.Timeline,
	spans []plannedSpan,
	joints []jointDecision,
	cache *sourceCache,
	totalFrames int64,
) (float64, error) {
	peak := 0.0
	err := forEachMixBlock(ctx, plan, spans, joints, cache, totalFrames, func(block []float32) error {
		for _, sample := range block {
			if value := math.Abs(float64(sample)); value > peak {
				peak = value
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return peak, nil
}
