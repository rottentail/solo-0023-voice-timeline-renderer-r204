package audiocodec

import (
	"encoding/binary"
	"errors"
	"math"
)

// EncodePCM16 encodes a complete interleaved float signal into a canonical
// 44-byte-header RIFF/WAVE payload.
func EncodePCM16(sampleRate, channels int, samples []float32) ([]byte, error) {
	frames, err := checkedFrames(sampleRate, channels, len(samples))
	if err != nil {
		return nil, err
	}
	header, err := EncodePCM16Header(sampleRate, channels, frames)
	if err != nil {
		return nil, err
	}
	result := make([]byte, len(header)+len(samples)*2)
	copy(result, header)
	EncodePCM16Block(samples, result[len(header):])
	return result, nil
}

// EncodePCM16Header builds the canonical 44-byte RIFF/WAVE header for a
// 16-bit PCM stream of frames frames. The same size bound as a full in-memory
// encode is enforced so streamed and buffered outputs stay interchangeable.
func EncodePCM16Header(sampleRate, channels int, frames int64) ([]byte, error) {
	if frames < 1 {
		return nil, errors.New("frame count must be at least one")
	}
	if err := validateStreamShape(sampleRate, channels); err != nil {
		return nil, err
	}
	dataBytes := uint64(frames) * uint64(channels) * 2
	if dataBytes > math.MaxUint32-36 {
		return nil, errors.New("encoded PCM payload is too large")
	}
	blockAlign := channels * 2
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataBytes))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], pcmFormatCode)
	binary.LittleEndian.PutUint16(header[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(sampleRate*blockAlign))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], pcmBitWidth)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataBytes))
	return header, nil
}

// EncodePCM16Block quantizes samples into dst as little-endian signed 16-bit
// PCM. dst must be exactly 2*len(samples) bytes.
func EncodePCM16Block(samples []float32, dst []byte) {
	for index, sample := range samples {
		quantized := Quantize16(sample)
		binary.LittleEndian.PutUint16(dst[index*2:index*2+2], uint16(quantized))
	}
}

// Quantize16 maps a float sample to the nearest 16-bit PCM code with the
// service's canonical asymmetric scaling.
func Quantize16(sample float32) int16 {
	var quantized int32
	switch {
	case sample <= -1:
		quantized = -32768
	case sample >= 1:
		quantized = 32767
	case sample < 0:
		quantized = int32(math.Round(float64(sample) * 32768))
	default:
		quantized = int32(math.Round(float64(sample) * 32767))
	}
	if quantized < -32768 {
		quantized = -32768
	}
	if quantized > 32767 {
		quantized = 32767
	}
	return int16(quantized)
}

// DecodeSample16 maps a 16-bit PCM code back to a float sample with the same
// scaling DecodeWAV applies, so measurements over quantized output match a
// full decode of the published bytes exactly.
func DecodeSample16(raw int16) float32 {
	if raw < 0 {
		return float32(raw) / 32768
	}
	return float32(raw) / 32767
}

func checkedFrames(sampleRate, channels, sampleCount int) (int64, error) {
	if err := validateStreamShape(sampleRate, channels); err != nil {
		return 0, err
	}
	if sampleCount == 0 || sampleCount%channels != 0 {
		return 0, errors.New("samples are empty or not frame-aligned")
	}
	return int64(sampleCount / channels), nil
}

func validateStreamShape(sampleRate, channels int) error {
	if sampleRate < 8000 || sampleRate > 192000 {
		return errors.New("sample rate is outside the accepted range")
	}
	if channels != 1 && channels != 2 {
		return errors.New("channel count must be one or two")
	}
	return nil
}
