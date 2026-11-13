package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// encodeResult captures everything the receipt needs about the published
// bytes, gathered while they were written — no decode-back pass, no second
// copy of the output in memory.
type encodeResult struct {
	Digest   string
	ByteSize int64
	Frames   int64
	Metrics  audiocodec.Metrics
}

// encodeMixed streams the mix a second time, applies the normalization gain
// while quantizing, and writes a canonical 16-bit PCM WAV to output.
// Measurements are accumulated over the quantized codes exactly as a decoder
// would read them back, so the receipt describes the published bytes
// precisely.
func encodeMixed(
	ctx context.Context,
	plan domain.Timeline,
	spans []plannedSpan,
	joints []jointDecision,
	cache *sourceCache,
	totalFrames int64,
	output io.Writer,
	gain float64,
) (encodeResult, error) {
	header, err := audiocodec.EncodePCM16Header(plan.SampleRate, plan.OutputChannels, totalFrames)
	if err != nil {
		return encodeResult{}, err
	}
	digest := sha256.New()
	writer := io.MultiWriter(output, digest)
	if _, err := writer.Write(header); err != nil {
		return encodeResult{}, err
	}
	var metrics audiocodec.MetricsAccumulator
	channels := plan.OutputChannels
	scaled := make([]float32, mixBlockFrames*channels)
	encoded := make([]byte, mixBlockFrames*channels*2)
	err = forEachMixBlock(ctx, plan, spans, joints, cache, totalFrames, func(block []float32) error {
		for index, sample := range block {
			scaled[index] = applyGain(sample, gain)
		}
		audiocodec.EncodePCM16Block(scaled[:len(block)], encoded[:len(block)*2])
		if _, err := writer.Write(encoded[:len(block)*2]); err != nil {
			return err
		}
		for index := range block {
			metrics.Add(audiocodec.DecodeSample16(audiocodec.Quantize16(scaled[index])))
		}
		return nil
	})
	if err != nil {
		return encodeResult{}, err
	}
	return encodeResult{
		Digest:   hex.EncodeToString(digest.Sum(nil)),
		ByteSize: int64(len(header)) + totalFrames*int64(channels)*2,
		Frames:   totalFrames,
		Metrics:  metrics.Metrics(),
	}, nil
}
