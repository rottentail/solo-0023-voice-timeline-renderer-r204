package checkflow

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// rssBudgetBytes is the whole-process resident memory budget the render
// pipeline must respect while handling the legal maximum of 64 spans.
const rssBudgetBytes = 128 << 20

// runCrossfadeQuality proves, with independently synthesized signals, that
// crossfade seams stay flat and explainable for correlated and uncorrelated
// content, that unavoidable cancellation is reported rather than hidden, and
// that a maximum-size timeline renders inside the service RSS budget.
func runCrossfadeQuality(ctx context.Context, harness *Harness) error {
	if err := checkCorrelatedSeam(ctx, harness); err != nil {
		return err
	}
	if err := checkUncorrelatedSeam(ctx, harness); err != nil {
		return err
	}
	if err := checkCancellationSeam(ctx, harness); err != nil {
		return err
	}
	if err := checkDoubleSidedFades(ctx, harness); err != nil {
		return err
	}
	if err := checkRepeatedReferences(ctx, harness); err != nil {
		return err
	}
	if err := checkMaxTimelineResources(ctx, harness); err != nil {
		return err
	}
	return nil
}

// checkCorrelatedSeam renders a joint whose overlap is the identical take on
// both sides (correlation ≈ 1). The historical equal-power law bulges +3.01 dB
// at the seam center here; the adaptive law must hold the seam within ±1 dB
// of the source segment's own level.
func checkCorrelatedSeam(ctx context.Context, harness *Harness) error {
	const rate = 48000
	shared := mixtureSamples(24000, rate, [][2]float64{{440, 0.3}, {1177, 0.2}})
	sourceA := concatSamples(toneSamples(24000, rate, 220, 0.5), shared)
	sourceB := concatSamples(shared, toneSamples(24000, rate, 330, 0.5))
	if err := uploadMonoSource(ctx, harness, "quality_corr_a", sourceA); err != nil {
		return err
	}
	if err := uploadMonoSource(ctx, harness, "quality_corr_b", sourceB); err != nil {
		return err
	}
	spans := []domain.TimelineSpan{
		{SourceID: "quality_corr_a", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0, CrossfadeFrames: 0},
		{SourceID: "quality_corr_b", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0, CrossfadeFrames: 24000},
	}
	receipt, clip, err := renderAndDecode(ctx, harness, "timeline_quality_corr", "render_quality_corr", spans, 1)
	if err != nil {
		return err
	}
	if err := expectPlacements(receipt, []int64{48000, 48000}, []int64{0, 24000}); err != nil {
		return fmt.Errorf("correlated seam placement: %w", err)
	}
	if len(receipt.Transitions) != 1 {
		return fmt.Errorf("correlated seam: expected 1 transition, got %d", len(receipt.Transitions))
	}
	transition := receipt.Transitions[0]
	if transition.Correlation < 0.99 {
		return fmt.Errorf("correlated seam: measured correlation %.4f, want >= 0.99", transition.Correlation)
	}
	if transition.Curve != "correlation_adaptive" {
		return fmt.Errorf("correlated seam: curve %q, want correlation_adaptive", transition.Curve)
	}
	if transition.Exponent < 1.95 || transition.Exponent > 2.0 {
		return fmt.Errorf("correlated seam: exponent %.4f outside [1.95, 2.0]", transition.Exponent)
	}
	if transition.Conflict != "" {
		return fmt.Errorf("correlated seam: unexpected conflict %q", transition.Conflict)
	}
	gain := math.Pow(10, receipt.Normalization.AppliedGainDB/20)
	expected := rmsDBFS(shared, 0, len(shared), 1) + 20*math.Log10(gain)
	// Two windows across the middle half of the seam; the equal-power law
	// would sit +3.01 dB hot here.
	for _, window := range [][2]int{{30000, 36000}, [2]int{36000, 42000}} {
		measured := rmsDBFS(clip.Samples, window[0], window[1], 1)
		if math.Abs(measured-expected) > 1.0 {
			return fmt.Errorf("correlated seam: level %.2f dBFS in [%d,%d) deviates %.2f dB from expected %.2f dBFS",
				measured, window[0], window[1], measured-expected, expected)
		}
	}
	return nil
}

// checkUncorrelatedSeam renders a joint of two independent noise signals. The
// equal-power law is already correct here and must be retained: the seam
// level must match the solo regions within ±1 dB.
func checkUncorrelatedSeam(ctx context.Context, harness *Harness) error {
	sourceA := noiseSamples(48000, 101, 0.4)
	sourceB := noiseSamples(48000, 202, 0.4)
	if err := uploadMonoSource(ctx, harness, "quality_uncorr_a", sourceA); err != nil {
		return err
	}
	if err := uploadMonoSource(ctx, harness, "quality_uncorr_b", sourceB); err != nil {
		return err
	}
	spans := []domain.TimelineSpan{
		{SourceID: "quality_uncorr_a", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0, CrossfadeFrames: 0},
		{SourceID: "quality_uncorr_b", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0, CrossfadeFrames: 24000},
	}
	receipt, clip, err := renderAndDecode(ctx, harness, "timeline_quality_uncorr", "render_quality_uncorr", spans, 1)
	if err != nil {
		return err
	}
	if err := expectPlacements(receipt, []int64{48000, 48000}, []int64{0, 24000}); err != nil {
		return fmt.Errorf("uncorrelated seam placement: %w", err)
	}
	if len(receipt.Transitions) != 1 {
		return fmt.Errorf("uncorrelated seam: expected 1 transition, got %d", len(receipt.Transitions))
	}
	transition := receipt.Transitions[0]
	if transition.Curve != "equal_power" || transition.Exponent != 1 {
		return fmt.Errorf("uncorrelated seam: curve %q exponent %.4f, want equal_power 1.0", transition.Curve, transition.Exponent)
	}
	if math.Abs(transition.Correlation) > 0.05 {
		return fmt.Errorf("uncorrelated seam: measured correlation %.4f, want |rho| <= 0.05", transition.Correlation)
	}
	soloA := rmsDBFS(clip.Samples, 6000, 18000, 1)
	seam := rmsDBFS(clip.Samples, 30000, 42000, 1)
	soloB := rmsDBFS(clip.Samples, 54000, 66000, 1)
	reference := (soloA + soloB) / 2
	if math.Abs(seam-reference) > 1.0 {
		return fmt.Errorf("uncorrelated seam: seam level %.2f dBFS deviates %.2f dB from solo reference %.2f dBFS",
			seam, seam-reference, reference)
	}
	return nil
}

// checkCancellationSeam renders a joint whose overlap is polarity-inverted on
// the incoming side (correlation ≈ -1). No amplitude law can keep that seam
// flat, so the renderer must keep the equal-power floor, keep the dip, keep
// the polarity, and say so on the receipt.
func checkCancellationSeam(ctx context.Context, harness *Harness) error {
	const rate = 48000
	shared := noiseSamples(24000, 303, 0.4)
	sourceA := concatSamples(toneSamples(24000, rate, 220, 0.5), shared)
	sourceB := concatSamples(negateSamples(shared), toneSamples(24000, rate, 330, 0.5))
	if err := uploadMonoSource(ctx, harness, "quality_anti_a", sourceA); err != nil {
		return err
	}
	if err := uploadMonoSource(ctx, harness, "quality_anti_b", sourceB); err != nil {
		return err
	}
	spans := []domain.TimelineSpan{
		{SourceID: "quality_anti_a", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0, CrossfadeFrames: 0},
		{SourceID: "quality_anti_b", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0, CrossfadeFrames: 24000},
	}
	receipt, clip, err := renderAndDecode(ctx, harness, "timeline_quality_anti", "render_quality_anti", spans, 1)
	if err != nil {
		return err
	}
	if err := expectPlacements(receipt, []int64{48000, 48000}, []int64{0, 24000}); err != nil {
		return fmt.Errorf("cancellation seam placement: %w", err)
	}
	if len(receipt.Transitions) != 1 {
		return fmt.Errorf("cancellation seam: expected 1 transition, got %d", len(receipt.Transitions))
	}
	transition := receipt.Transitions[0]
	if transition.Conflict != "cancellation" {
		return fmt.Errorf("cancellation seam: conflict %q, want cancellation", transition.Conflict)
	}
	if transition.Correlation > -0.3 {
		return fmt.Errorf("cancellation seam: measured correlation %.4f, want <= -0.3", transition.Correlation)
	}
	if transition.Exponent != 1 || transition.Curve != "equal_power" {
		return fmt.Errorf("cancellation seam: curve %q exponent %.4f, want clamped equal_power 1.0",
			transition.Curve, transition.Exponent)
	}
	warningFound := false
	for _, warning := range receipt.Warnings {
		if strings.Contains(warning, "anti-correlated") {
			warningFound = true
		}
	}
	if !warningFound {
		return fmt.Errorf("cancellation seam: no anti-correlated warning in %v", receipt.Warnings)
	}
	// The incoming span's solo region must keep its recorded polarity: the
	// renderer may not flip the sign to manufacture a flat seam.
	gain := math.Pow(10, receipt.Normalization.AppliedGainDB/20)
	soloOut := clip.Samples[54000:66000]
	soloSource := make([]float32, len(soloOut))
	for index, sample := range sourceB[30000:42000] {
		soloSource[index] = float32(float64(sample) * gain)
	}
	if pearsonMono(soloOut, soloSource) < 0.99 {
		return fmt.Errorf("cancellation seam: incoming solo region polarity was altered")
	}
	// The dip itself must remain: flattening it would mean the cancellation
	// was hidden instead of preserved.
	solo := rmsDBFS(clip.Samples, 54000, 66000, 1)
	seamCenter := rmsDBFS(clip.Samples, 33000, 39000, 1)
	if solo-seamCenter < 6 {
		return fmt.Errorf("cancellation seam: seam center %.2f dBFS is only %.2f dB below solo %.2f dBFS; dip was manufactured away",
			seamCenter, solo-seamCenter, solo)
	}
	return nil
}

// checkDoubleSidedFades renders a short middle span whose incoming and
// outgoing fades overlap, so three spans are audible at once. Every source's
// influence range must still match the project placement and the receipt.
func checkDoubleSidedFades(ctx context.Context, harness *Harness) error {
	sourceA := noiseSamples(48000, 401, 0.35)
	sourceB := noiseSamples(28800, 402, 0.35)
	sourceC := noiseSamples(48000, 403, 0.35)
	if err := uploadMonoSource(ctx, harness, "quality_dual_a", sourceA); err != nil {
		return err
	}
	if err := uploadMonoSource(ctx, harness, "quality_dual_b", sourceB); err != nil {
		return err
	}
	if err := uploadMonoSource(ctx, harness, "quality_dual_c", sourceC); err != nil {
		return err
	}
	spans := []domain.TimelineSpan{
		{SourceID: "quality_dual_a", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0, CrossfadeFrames: 0},
		{SourceID: "quality_dual_b", SourceStartFrame: 0, SourceEndFrame: 28800, GainDB: 0, CrossfadeFrames: 16800},
		{SourceID: "quality_dual_c", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0, CrossfadeFrames: 16800},
	}
	receipt, clip, err := renderAndDecode(ctx, harness, "timeline_quality_dual", "render_quality_dual", spans, 1)
	if err != nil {
		return err
	}
	if err := expectPlacements(receipt, []int64{48000, 28800, 48000}, []int64{0, 16800, 16800}); err != nil {
		return fmt.Errorf("double-sided fades placement: %w", err)
	}
	if clip.FrameCount() != receipt.Output.FrameCount {
		return fmt.Errorf("double-sided fades: decoded frames %d != receipt %d", clip.FrameCount(), receipt.Output.FrameCount)
	}
	if len(receipt.Transitions) != 2 {
		return fmt.Errorf("double-sided fades: expected 2 transitions, got %d", len(receipt.Transitions))
	}
	for index, transition := range receipt.Transitions {
		if transition.CrossfadeFrames != 16800 || transition.SpanIndex != index+1 {
			return fmt.Errorf("double-sided fades: transition %+v does not match the project", transition)
		}
	}
	if math.Abs(receipt.Output.PeakDBFS-(-1)) > 0.15 {
		return fmt.Errorf("double-sided fades: output peak %.3f dBFS misses the -1 dBFS target", receipt.Output.PeakDBFS)
	}
	return nil
}

// checkRepeatedReferences places several joints over the same single source.
// Each joint must be decided by its own overlap: the first joint aligns an
// identical noise segment (correlation ≈ 1), the second aligns tone against
// independent noise (correlation ≈ 0). If joints inherited analysis state
// from each other — or from the shared source — the two curves would match.
func checkRepeatedReferences(ctx context.Context, harness *Harness) error {
	const rate = 48000
	shared := noiseSamples(24000, 707, 0.4)
	source := concatSamples(
		toneSamples(24000, rate, 440, 0.4),
		shared,
		toneSamples(24000, rate, 440, 0.4),
		noiseSamples(24000, 808, 0.4),
	)
	if err := uploadMonoSource(ctx, harness, "quality_repeat", source); err != nil {
		return err
	}
	spans := []domain.TimelineSpan{
		{SourceID: "quality_repeat", SourceStartFrame: 0, SourceEndFrame: 48000, GainDB: 0, CrossfadeFrames: 0},
		{SourceID: "quality_repeat", SourceStartFrame: 24000, SourceEndFrame: 72000, GainDB: 0, CrossfadeFrames: 24000},
		{SourceID: "quality_repeat", SourceStartFrame: 72000, SourceEndFrame: 96000, GainDB: 0, CrossfadeFrames: 24000},
	}
	receipt, _, err := renderAndDecode(ctx, harness, "timeline_quality_repeat", "render_quality_repeat", spans, 1)
	if err != nil {
		return err
	}
	if err := expectPlacements(receipt, []int64{48000, 48000, 24000}, []int64{0, 24000, 24000}); err != nil {
		return fmt.Errorf("repeated references placement: %w", err)
	}
	if len(receipt.Transitions) != 2 {
		return fmt.Errorf("repeated references: expected 2 transitions, got %d", len(receipt.Transitions))
	}
	first, second := receipt.Transitions[0], receipt.Transitions[1]
	if first.Curve != "correlation_adaptive" || first.Correlation < 0.95 {
		return fmt.Errorf("repeated references: joint 1 curve %q correlation %.4f, want adaptive >= 0.95",
			first.Curve, first.Correlation)
	}
	if second.Curve != "equal_power" || second.Exponent != 1 {
		return fmt.Errorf("repeated references: joint 2 curve %q exponent %.4f, want equal_power 1.0",
			second.Curve, second.Exponent)
	}
	return nil
}

// checkMaxTimelineResources renders the legal maximum of 64 spans with long
// crossfades, repeated source references, and overlapped double-sided fades,
// then proves the whole-process RSS stayed inside 128 MiB and that re-renders
// reproduce the exact same output bytes.
func checkMaxTimelineResources(ctx context.Context, harness *Harness) error {
	const (
		rate         = 48000
		sourceFrames = 220000
		longFrames   = 210000
		shortFrames  = 70000
		crossfade    = 60000
	)
	// Two independent noise beds plus a tone pair whose period divides the
	// joint offset, so the timeline exercises equal-power and adaptive joints
	// at scale. Sources are referenced repeatedly across the 64 spans.
	toneL := toneSamples(sourceFrames, rate, 320, 0.3)
	toneR := toneSamples(sourceFrames, rate, 240, 0.24)
	sources := []struct {
		id      string
		samples []float32
	}{
		{"quality_max_a", stereoSamples(noiseSamples(sourceFrames, 11, 0.35), noiseSamples(sourceFrames, 12, 0.35))},
		{"quality_max_b", stereoSamples(noiseSamples(sourceFrames, 21, 0.35), noiseSamples(sourceFrames, 22, 0.35))},
		{"quality_max_c", stereoSamples(toneL, toneR)},
	}
	for _, source := range sources {
		wav, err := encodeStereoWAV(rate, source.samples)
		if err != nil {
			return err
		}
		if _, status, err := harness.createSource(ctx, source.id, "quality max source", wav); err != nil || status != 201 {
			return fmt.Errorf("max timeline source %s: status=%d err=%v", source.id, status, err)
		}
	}
	spans := make([]domain.TimelineSpan, 0, 64)
	spanFrames := make([]int64, 0, 64)
	crossfades := make([]int64, 0, 64)
	sourceIDs := []string{"quality_max_a", "quality_max_b", "quality_max_c", "quality_max_c"}
	for index := 0; index < 64; index++ {
		frames := int64(longFrames)
		if index%8 == 7 {
			frames = shortFrames
		}
		xf := int64(crossfade)
		if index == 0 {
			xf = 0
		}
		spans = append(spans, domain.TimelineSpan{
			SourceID:         sourceIDs[index%4],
			SourceStartFrame: 0,
			SourceEndFrame:   frames,
			GainDB:           0,
			CrossfadeFrames:  xf,
		})
		spanFrames = append(spanFrames, frames)
		crossfades = append(crossfades, xf)
	}
	if _, status, err := harness.createTimeline(ctx, "timeline_quality_max", "max size timeline", rate, 2, -1, spans); err != nil || status != 201 {
		return fmt.Errorf("max timeline compose: status=%d err=%v", status, err)
	}
	job, status, err := harness.createRender(ctx, "render_quality_max", "timeline_quality_max")
	if err != nil || status != 201 {
		return fmt.Errorf("max timeline render: status=%d err=%v", status, err)
	}
	if job.State != domain.RenderCompleted {
		return fmt.Errorf("max timeline render state %s", job.State)
	}
	receipt, status, err := harness.getReceipt(ctx, "render_quality_max")
	if err != nil || status != 200 {
		return fmt.Errorf("max timeline receipt: status=%d err=%v", status, err)
	}
	if err := expectPlacements(receipt, spanFrames, crossfades); err != nil {
		return fmt.Errorf("max timeline placement: %w", err)
	}
	if len(receipt.Transitions) != 63 {
		return fmt.Errorf("max timeline: expected 63 transitions, got %d", len(receipt.Transitions))
	}
	adaptive := 0
	for _, transition := range receipt.Transitions {
		if transition.CrossfadeFrames != crossfade {
			return fmt.Errorf("max timeline: transition %+v has wrong crossfade length", transition)
		}
		if transition.Exponent < 1 || transition.Exponent > 2 {
			return fmt.Errorf("max timeline: transition exponent %.4f outside [1, 2]", transition.Exponent)
		}
		if transition.Curve == "correlation_adaptive" {
			adaptive++
		}
	}
	if adaptive == 0 {
		return fmt.Errorf("max timeline: no correlation-adaptive joint was exercised")
	}

	// Repeating the same render identifier is idempotent and must return the
	// same completed job.
	replay, status, err := harness.createRender(ctx, "render_quality_max", "timeline_quality_max")
	if err != nil || status != 200 {
		return fmt.Errorf("max timeline idempotent replay: status=%d err=%v", status, err)
	}
	if replay.Output == nil || replay.Output.Digest != receipt.Output.Digest {
		return fmt.Errorf("max timeline idempotent replay returned a different output")
	}

	// A render cancelled mid-flight must reach a terminal state (or never
	// have existed), and a fresh redo must not inherit anything from it.
	cancelCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, _, _ = harness.createRender(cancelCtx, "render_quality_cancel", "timeline_quality_max")
	deadline := time.Now().Add(10 * time.Second)
	for {
		cancelled, status, err := harness.getRender(ctx, "render_quality_cancel")
		if err == nil && status == 404 {
			break
		}
		if err == nil && status == 200 &&
			(cancelled.State == domain.RenderCompleted || cancelled.State == domain.RenderFailed) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cancelled render did not reach a terminal state")
		}
		time.Sleep(50 * time.Millisecond)
	}
	redo, status, err := harness.createRender(ctx, "render_quality_redo", "timeline_quality_max")
	if err != nil || status != 201 {
		return fmt.Errorf("max timeline redo: status=%d err=%v", status, err)
	}
	if redo.State != domain.RenderCompleted || redo.Output == nil {
		return fmt.Errorf("max timeline redo state %s", redo.State)
	}
	if redo.Output.Digest != receipt.Output.Digest {
		return fmt.Errorf("max timeline redo digest %s != reference %s; analysis state leaked between renders",
			redo.Output.Digest, receipt.Output.Digest)
	}

	peak, err := peakRSSBytes()
	if err != nil {
		return err
	}
	if peak > rssBudgetBytes {
		return fmt.Errorf("max timeline: peak process RSS %.1f MiB exceeds the %.0f MiB budget",
			float64(peak)/(1<<20), float64(rssBudgetBytes)/(1<<20))
	}
	fmt.Printf("crossfade-quality: 64-span render peak process RSS %.1f MiB (budget %.0f MiB)\n",
		float64(peak)/(1<<20), float64(rssBudgetBytes)/(1<<20))
	return nil
}

// uploadMonoSource registers a mono 48 kHz source built from check-side
// samples.
func uploadMonoSource(ctx context.Context, harness *Harness, id string, samples []float32) error {
	wav, err := encodeMonoWAV(48000, samples)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, id, "quality source", wav); err != nil || status != 201 {
		return fmt.Errorf("source %s upload: status=%d err=%v", id, status, err)
	}
	return nil
}

// renderAndDecode composes a two-or-three-span mono timeline, renders it, and
// returns the receipt plus the decoded output for independent measurement.
func renderAndDecode(
	ctx context.Context,
	harness *Harness,
	timelineID string,
	renderID string,
	spans []domain.TimelineSpan,
	channels int,
) (domain.RenderReceipt, *audiocodec.Clip, error) {
	if _, status, err := harness.createTimeline(ctx, timelineID, "quality timeline", 48000, channels, -1, spans); err != nil || status != 201 {
		return domain.RenderReceipt{}, nil, fmt.Errorf("timeline %s: status=%d err=%v", timelineID, status, err)
	}
	job, status, err := harness.createRender(ctx, renderID, timelineID)
	if err != nil || status != 201 {
		return domain.RenderReceipt{}, nil, fmt.Errorf("render %s: status=%d err=%v", renderID, status, err)
	}
	if job.State != domain.RenderCompleted {
		return domain.RenderReceipt{}, nil, fmt.Errorf("render %s finished in state %s", renderID, job.State)
	}
	receipt, status, err := harness.getReceipt(ctx, renderID)
	if err != nil || status != 200 {
		return domain.RenderReceipt{}, nil, fmt.Errorf("receipt %s: status=%d err=%v", renderID, status, err)
	}
	output, status, err := harness.getOutput(ctx, renderID)
	if err != nil {
		return domain.RenderReceipt{}, nil, fmt.Errorf("output %s: %w", renderID, err)
	}
	clip, _, err := audiocodec.DecodeWAV(output)
	if err != nil {
		return domain.RenderReceipt{}, nil, fmt.Errorf("decode output %s: %w", renderID, err)
	}
	return receipt, clip, nil
}

// expectPlacements independently recomputes span placement from the project
// (span lengths and crossfades) and requires the receipt — and the final
// frame count — to match it exactly.
func expectPlacements(receipt domain.RenderReceipt, spanFrames []int64, crossfades []int64) error {
	if len(receipt.Spans) != len(spanFrames) {
		return fmt.Errorf("receipt has %d spans, want %d", len(receipt.Spans), len(spanFrames))
	}
	cursor := int64(0)
	for index, frames := range spanFrames {
		start := cursor
		if index > 0 {
			start -= crossfades[index]
		}
		end := start + frames
		cursor = end
		span := receipt.Spans[index]
		if span.OutputStartFrame != start || span.OutputEndFrame != end || span.OutputFrames != frames {
			return fmt.Errorf("span %d placed at [%d,%d) len %d, want [%d,%d) len %d",
				index, span.OutputStartFrame, span.OutputEndFrame, span.OutputFrames, start, end, frames)
		}
	}
	if receipt.Output.FrameCount != cursor {
		return fmt.Errorf("output frames %d, want %d", receipt.Output.FrameCount, cursor)
	}
	return nil
}
