package audiocodec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// maxFMTChunkBytes bounds the fmt chunk payload inspected while parsing a
// header. PCM format fields live in the first 16 bytes; anything beyond a
// generous extension allowance is rejected instead of buffered.
const maxFMTChunkBytes = 1 << 20

// FrameReader gives random-access, windowed reads over a stored 16-bit PCM
// WAV payload. It exists so renders can pull bounded frame windows instead of
// decoding an entire source into memory. A FrameReader is not safe for
// concurrent use: it reuses one conversion scratch buffer across reads.
type FrameReader struct {
	reader     io.ReaderAt
	closer     io.Closer
	info       WAVInfo
	dataOffset int64
	blockAlign int
	raw        []byte
}

// OpenFrameReader parses the RIFF/WAVE structure from a random-access payload
// of size bytes. The closer is closed when the reader is closed; it may be
// nil. Validation mirrors DecodeWAV so a payload accepted for full decoding is
// accepted here with identical semantics.
func OpenFrameReader(reader io.ReaderAt, size int64, closer io.Closer) (*FrameReader, error) {
	if reader == nil {
		return nil, errors.New("frame reader requires a random-access payload")
	}
	if size < 12 {
		return nil, errors.New("file is shorter than a RIFF header")
	}
	header := make([]byte, 12)
	if _, err := reader.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("cannot read RIFF header: %w", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return nil, errors.New("file is not a RIFF/WAVE payload")
	}
	riffSize := uint64(binary.LittleEndian.Uint32(header[4:8]))
	if riffSize+8 > uint64(size) {
		return nil, errors.New("RIFF size exceeds the supplied payload")
	}

	var formatChunk []byte
	var dataOffset, dataSize int64
	offset := int64(12)
	chunkHeader := make([]byte, 8)
	for offset+8 <= size {
		if _, err := reader.ReadAt(chunkHeader, offset); err != nil {
			return nil, fmt.Errorf("cannot read chunk header at %d: %w", offset, err)
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := int64(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		payloadStart := offset + 8
		payloadEnd := payloadStart + chunkSize
		if chunkSize < 0 || payloadEnd < payloadStart || payloadEnd > size {
			return nil, fmt.Errorf("chunk %q exceeds the payload", chunkID)
		}
		switch chunkID {
		case "fmt ":
			if chunkSize > maxFMTChunkBytes {
				return nil, fmt.Errorf("fmt chunk of %d bytes exceeds the supported limit", chunkSize)
			}
			formatChunk = make([]byte, chunkSize)
			if _, err := reader.ReadAt(formatChunk, payloadStart); err != nil {
				return nil, fmt.Errorf("cannot read fmt chunk: %w", err)
			}
		case "data":
			dataOffset = payloadStart
			dataSize = chunkSize
		}
		offset = payloadEnd
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if len(formatChunk) < 16 {
		return nil, errors.New("fmt chunk is missing or too short")
	}
	if dataOffset == 0 {
		return nil, errors.New("data chunk is missing")
	}

	audioFormat := binary.LittleEndian.Uint16(formatChunk[0:2])
	channels := int(binary.LittleEndian.Uint16(formatChunk[2:4]))
	sampleRate := int(binary.LittleEndian.Uint32(formatChunk[4:8]))
	byteRate := binary.LittleEndian.Uint32(formatChunk[8:12])
	blockAlign := int(binary.LittleEndian.Uint16(formatChunk[12:14]))
	bitsPerSample := int(binary.LittleEndian.Uint16(formatChunk[14:16]))

	if audioFormat != pcmFormatCode {
		return nil, fmt.Errorf("unsupported WAV format code %d", audioFormat)
	}
	if bitsPerSample != pcmBitWidth {
		return nil, fmt.Errorf("unsupported PCM width %d", bitsPerSample)
	}
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("unsupported channel count %d", channels)
	}
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, fmt.Errorf("sample rate %d is outside the accepted range", sampleRate)
	}
	expectedBlockAlign := channels * (bitsPerSample / 8)
	if blockAlign != expectedBlockAlign {
		return nil, errors.New("block alignment does not match channels and sample width")
	}
	expectedByteRate := uint32(sampleRate * blockAlign)
	if byteRate != expectedByteRate {
		return nil, errors.New("byte rate does not match sample rate and block alignment")
	}
	if dataSize == 0 || dataSize%int64(blockAlign) != 0 {
		return nil, errors.New("sample data is empty or not frame-aligned")
	}

	frameCount := dataSize / int64(blockAlign)
	return &FrameReader{
		reader: reader,
		closer: closer,
		info: WAVInfo{
			SampleRate:    sampleRate,
			Channels:      channels,
			BitsPerSample: bitsPerSample,
			FrameCount:    frameCount,
			ByteCount:     size,
		},
		dataOffset: dataOffset,
		blockAlign: blockAlign,
	}, nil
}

// Info returns the parsed stream description.
func (reader *FrameReader) Info() WAVInfo {
	return reader.info
}

// ReadFrames fills dst with len(dst)/channels frames starting at startFrame.
// Samples are converted with the exact scaling DecodeWAV uses so windowed
// reads and full decodes produce identical float values.
func (reader *FrameReader) ReadFrames(startFrame int64, dst []float32) error {
	channels := reader.info.Channels
	if len(dst) == 0 || len(dst)%channels != 0 {
		return errors.New("destination is empty or not frame-aligned")
	}
	frames := int64(len(dst) / channels)
	if startFrame < 0 || startFrame+frames > reader.info.FrameCount {
		return errors.New("frame interval is outside the clip")
	}
	need := frames * int64(reader.blockAlign)
	if int64(cap(reader.raw)) < need {
		reader.raw = make([]byte, need)
	}
	raw := reader.raw[:need]
	if _, err := reader.reader.ReadAt(raw, reader.dataOffset+startFrame*int64(reader.blockAlign)); err != nil {
		return fmt.Errorf("cannot read frame window: %w", err)
	}
	for index := range dst {
		value := int16(binary.LittleEndian.Uint16(raw[index*2 : index*2+2]))
		dst[index] = DecodeSample16(value)
	}
	return nil
}

// Close releases the underlying handle.
func (reader *FrameReader) Close() error {
	if reader.closer == nil {
		return nil
	}
	return reader.closer.Close()
}
