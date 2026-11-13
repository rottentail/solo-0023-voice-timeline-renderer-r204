package render

import (
	"fmt"
	"time"

	"voice-timeline-renderer/internal/domain"
)

// receiptSchemaVersion is bumped whenever the receipt shape changes. Version
// 2 adds the per-joint transition report.
const receiptSchemaVersion = 2

func buildReceipt(
	renderID string,
	plan domain.Timeline,
	spans []plannedSpan,
	joints []jointDecision,
	beforePeakDBFS float64,
	gainDB float64,
	encoded encodeResult,
	now time.Time,
) (domain.RenderReceipt, domain.OutputAsset, error) {
	output := domain.OutputAsset{
		Digest:        encoded.Digest,
		ByteCount:     encoded.ByteSize,
		SampleRate:    plan.SampleRate,
		Channels:      plan.OutputChannels,
		BitsPerSample: 16,
		FrameCount:    encoded.Frames,
		DurationMS:    framesToMilliseconds(encoded.Frames, plan.SampleRate),
		PeakDBFS:      encoded.Metrics.PeakDBFS,
		RMSDBFS:       encoded.Metrics.RMSDBFS,
	}
	receiptSpans := make([]domain.ReceiptSpan, 0, len(spans))
	transitions := make([]domain.ReceiptTransition, 0, len(spans))
	warnings := make([]string, 0)
	for _, span := range spans {
		receiptSpans = append(receiptSpans, domain.ReceiptSpan{
			Index:            span.Index,
			SourceID:         span.SourceID,
			SourceDigest:     span.SourceDigest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: span.OutputStartFrame,
			OutputEndFrame:   span.OutputEndFrame,
			OutputFrames:     span.OutputFrames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    span.ChannelAction,
		})
		if span.ChannelAction != "unchanged" {
			warnings = append(warnings, fmt.Sprintf("span %d applied %s", span.Index, span.ChannelAction))
		}
		joint := joints[span.Index]
		if span.Index == 0 || joint.Frames <= 0 {
			continue
		}
		transitions = append(transitions, domain.ReceiptTransition{
			SpanIndex:       span.Index,
			CrossfadeFrames: joint.Frames,
			Correlation:     joint.Correlation,
			Curve:           joint.Curve,
			Exponent:        joint.Exponent,
			Conflict:        joint.Conflict,
		})
		if joint.Conflict == conflictCancel {
			warnings = append(warnings, fmt.Sprintf(
				"span %d crossfade: anti-correlated overlap (correlation %.3f); equal-power floor retained and the residual seam dip is inherent to the content",
				span.Index, joint.Correlation,
			))
		}
	}
	receipt := domain.RenderReceipt{
		SchemaVersion: receiptSchemaVersion,
		RenderID:      renderID,
		TimelineID:    plan.ID,
		Output:        output,
		Normalization: domain.NormalizationReceipt{
			RequestedPeakDBFS: plan.TargetPeakDBFS,
			BeforePeakDBFS:    beforePeakDBFS,
			AppliedGainDB:     gainDB,
			OutputPeakDBFS:    encoded.Metrics.PeakDBFS,
		},
		Spans:       receiptSpans,
		Transitions: transitions,
		Warnings:    warnings,
		CreatedAt:   now.UTC(),
	}
	return receipt, output, nil
}

func framesToMilliseconds(frames int64, sampleRate int) int64 {
	if frames <= 0 || sampleRate <= 0 {
		return 0
	}
	return int64(float64(frames) * 1000 / float64(sampleRate))
}
