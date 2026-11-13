package render

import (
	"context"
	"math"

	"voice-timeline-renderer/internal/domain"
)

// analysisBlockFrames bounds the audio held while measuring one joint. A
// joint of any length — up to the full legal timeline duration — is reduced
// to a fixed set of running statistics, never a retained copy of the overlap.
const analysisBlockFrames = 16384

// Fade laws recorded in the receipt.
const (
	curveEqualPower  = "equal_power"
	curveAdaptive    = "correlation_adaptive"
	conflictCancel   = "cancellation"
	adaptiveFloorRho = 0.05
	conflictRho      = -0.3
)

// correlationAccumulator tracks the zero-lag normalized cross-correlation
// between the outgoing and incoming overlap signals with West's incremental
// mean/covariance algorithm: single pass, numerically stable, O(1) memory.
type correlationAccumulator struct {
	count float64
	meanA float64
	meanB float64
	varA  float64
	varB  float64
	covar float64
}

func (acc *correlationAccumulator) add(outgoing, incoming []float32) {
	for index, a := range outgoing {
		b := incoming[index]
		acc.count++
		deltaA := float64(a) - acc.meanA
		acc.meanA += deltaA / acc.count
		deltaB := float64(b) - acc.meanB
		acc.meanB += deltaB / acc.count
		acc.varA += deltaA * (float64(a) - acc.meanA)
		acc.varB += deltaB * (float64(b) - acc.meanB)
		acc.covar += deltaA * (float64(b) - acc.meanB)
	}
}

// correlation returns the Pearson coefficient in [-1, 1]. A degenerate
// overlap — silence on either side — is reported as 0 (uncorrelated), which
// selects the equal-power law already proven for content with nothing to
// align to.
func (acc *correlationAccumulator) correlation() float64 {
	if acc.count < 2 || acc.varA <= 0 || acc.varB <= 0 {
		return 0
	}
	rho := acc.covar / math.Sqrt(acc.varA*acc.varB)
	if rho > 1 {
		return 1
	}
	if rho < -1 {
		return -1
	}
	return rho
}

// analyzeJoints measures every crossfade joint in the timeline and derives
// its fade law. The incoming span's head and the outgoing span's tail are
// streamed in bounded blocks; only the resulting jointDecision scalars are
// kept, so 64 joints of any length cost a fixed few hundred bytes.
func analyzeJoints(
	ctx context.Context,
	plan domain.Timeline,
	spans []plannedSpan,
	cache *sourceCache,
) ([]jointDecision, error) {
	joints := make([]jointDecision, len(spans))
	channels := plan.OutputChannels
	factory := newWindowFactory(plan)
	outgoingBuf := make([]float32, analysisBlockFrames*channels)
	incomingBuf := make([]float32, analysisBlockFrames*channels)
	for index := 1; index < len(spans); index++ {
		frames := spans[index].CrossfadeFrames
		if frames <= 0 {
			continue
		}
		outgoing, err := cache.get(spans[index-1].SourceID)
		if err != nil {
			return nil, err
		}
		incoming, err := cache.get(spans[index].SourceID)
		if err != nil {
			return nil, err
		}
		var acc correlationAccumulator
		for offset := int64(0); offset < frames; offset += analysisBlockFrames {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			block := frames - offset
			if block > analysisBlockFrames {
				block = analysisBlockFrames
			}
			tailStart := spans[index-1].OutputFrames - frames + offset
			if err := factory.produce(outgoing, spans[index-1], tailStart, block, outgoingBuf); err != nil {
				return nil, err
			}
			if err := factory.produce(incoming, spans[index], offset, block, incomingBuf); err != nil {
				return nil, err
			}
			acc.add(outgoingBuf[:block*int64(channels)], incomingBuf[:block*int64(channels)])
		}
		joints[index] = decideJoint(frames, acc.correlation())
	}
	return joints, nil
}

// decideJoint maps a measured overlap correlation to a fade law.
//
// The law family is w_in = sin^γ(u·π/2), w_out = cos^γ(u·π/2). At the seam
// center the mixed power of two equal-level overlaps is 2·w²·(1+ρ) with
// w = (1/√2)^γ, so γ = 1 + log2(1+ρ) keeps the seam center at the same power
// as its neighbors for any ρ ≥ 0: γ = 1 is the classic equal-power law for
// uncorrelated content, γ = 2 the constant-amplitude law for identical
// content, and the family interpolates exactly between them.
//
// Negative ρ demands weights above the equal-power envelope to stay flat,
// which would pump levels and still fail as ρ → -1. That case cannot be
// satisfied by any amplitude law, so the equal-power floor is kept and the
// joint is flagged as a cancellation conflict instead of flipping polarity
// or moving dialogue to fake a flat seam.
func decideJoint(frames int64, rho float64) jointDecision {
	decision := jointDecision{
		Frames:      frames,
		Correlation: round6(rho),
		Exponent:    1,
		Curve:       curveEqualPower,
	}
	if rho <= conflictRho {
		decision.Conflict = conflictCancel
	}
	if rho > adaptiveFloorRho {
		exponent := 1 + math.Log2(1+rho)
		if exponent > 2 {
			exponent = 2
		}
		decision.Exponent = round6(exponent)
		decision.Curve = curveAdaptive
	}
	return decision
}

func round6(value float64) float64 {
	rounded := math.Round(value*1e6) / 1e6
	if rounded == 0 {
		return 0
	}
	return rounded
}
