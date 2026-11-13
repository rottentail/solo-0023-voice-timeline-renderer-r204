package render

import (
	"context"
	"errors"

	"voice-timeline-renderer/internal/domain"
)

// plannedSpan is the render-time placement of one timeline span. Placement is
// derived from source metadata only, so planning a 64-span timeline touches no
// audio bytes.
type plannedSpan struct {
	Index            int
	SourceID         string
	SourceDigest     string
	SourceStartFrame int64
	SourceEndFrame   int64
	OutputStartFrame int64
	OutputEndFrame   int64
	OutputFrames     int64
	CrossfadeFrames  int64
	GainDB           float64
	ChannelAction    string
}

// jointDecision is the per-joint fade law chosen from the measured overlap
// correlation. It is the only analysis state kept per joint: a handful of
// scalars, never audio. Decisions live and die inside a single render, so
// repeated source references and re-renders can never inherit another
// joint's analysis.
type jointDecision struct {
	Frames      int64
	Correlation float64
	Exponent    float64
	Curve       string
	Conflict    string
}

// planSpans computes output placement for every span from timeline and source
// metadata. The math is identical to the historical decoder-based planner:
// span output frames are the source interval scaled to the timeline rate, and
// each span after the first starts CrossfadeFrames before the running cursor.
func planSpans(
	ctx context.Context,
	plan domain.Timeline,
	provider SourceProvider,
) ([]plannedSpan, int64, error) {
	metas := make(map[string]domain.SourceAsset)
	prepared := make([]plannedSpan, 0, len(plan.Spans))
	cursor := int64(0)
	for index, span := range plan.Spans {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		meta, ok := metas[span.SourceID]
		if !ok {
			asset, err := provider.GetSourceMeta(span.SourceID)
			if err != nil {
				return nil, 0, err
			}
			meta = asset
			metas[span.SourceID] = asset
		}
		frames := domain.ScaledFrames(span.SourceEndFrame-span.SourceStartFrame, meta.SampleRate, plan.SampleRate)
		if frames < 1 {
			return nil, 0, errors.New("span produced no output frames")
		}

		start := cursor
		if index > 0 {
			start -= span.CrossfadeFrames
		}
		if start < 0 {
			return nil, 0, errors.New("crossfade produced a negative output position")
		}
		end := start + frames
		cursor = end

		prepared = append(prepared, plannedSpan{
			Index:            index,
			SourceID:         span.SourceID,
			SourceDigest:     meta.Digest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: start,
			OutputEndFrame:   end,
			OutputFrames:     frames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    channelAction(meta.Channels, plan.OutputChannels),
		})
	}
	return prepared, cursor, nil
}

func channelAction(sourceChannels, outputChannels int) string {
	switch {
	case sourceChannels == outputChannels:
		return "unchanged"
	case sourceChannels == 1 && outputChannels == 2:
		return "mono_to_stereo"
	default:
		return "stereo_to_mono"
	}
}
