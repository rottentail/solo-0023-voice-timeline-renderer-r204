package render

import (
	"context"
	"errors"
	"io"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

type Limits struct {
	MaxOutputFrames int64
}

type Result struct {
	Receipt domain.RenderReceipt
	Output  domain.OutputAsset
}

// Render executes one timeline into a staged output writer. The pipeline is
// memory-bounded at every stage: placement is planned from metadata, joint
// analysis streams bounded overlap blocks, sources are read through frame
// windows, the mix is streamed in blocks (one pass for the peak, one to
// encode), and the normalized output is quantized straight to the writer. No
// stage holds a full-length audio buffer, so a 64-span timeline at the legal
// frame limit renders inside the service RSS budget.
func Render(
	ctx context.Context,
	renderID string,
	plan domain.Timeline,
	provider SourceProvider,
	limits Limits,
	output io.Writer,
	now time.Time,
) (Result, error) {
	spans, totalFrames, err := planSpans(ctx, plan, provider)
	if err != nil {
		return Result{}, err
	}
	if totalFrames < 1 {
		return Result{}, errors.New("timeline produced no output frames")
	}
	if limits.MaxOutputFrames > 0 && totalFrames > limits.MaxOutputFrames {
		return Result{}, errors.New("rendered timeline exceeds the configured frame limit")
	}
	cache := newSourceCache(provider)
	defer cache.closeAll()
	joints, err := analyzeJoints(ctx, plan, spans, cache)
	if err != nil {
		return Result{}, err
	}
	peak, err := mixPeak(ctx, plan, spans, joints, cache, totalFrames)
	if err != nil {
		return Result{}, err
	}
	gain := normalizationGain(peak, plan.TargetPeakDBFS)
	encoded, err := encodeMixed(ctx, plan, spans, joints, cache, totalFrames, output, gain)
	if err != nil {
		return Result{}, err
	}
	receipt, outputAsset, err := buildReceipt(
		renderID,
		plan,
		spans,
		joints,
		audiocodec.LinearToDB(peak),
		audiocodec.LinearToDB(gain),
		encoded,
		now,
	)
	if err != nil {
		return Result{}, err
	}
	return Result{Receipt: receipt, Output: outputAsset}, nil
}
