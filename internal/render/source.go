package render

import (
	"crypto/sha256"
	"encoding/hex"
	"io"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// SourceProvider is the storage-facing contract the renderer needs. Media
// handles must additionally implement io.ReaderAt so frame windows can be
// read without buffering whole sources.
type SourceProvider interface {
	GetSourceMeta(id string) (domain.SourceAsset, error)
	OpenSourceMedia(id string) (io.ReadCloser, error)
}

// sourceHandle couples one source's verified metadata with its random-access
// frame reader. Handles live for the duration of a single render only.
type sourceHandle struct {
	asset  domain.SourceAsset
	frames *audiocodec.FrameReader
}

// sourceCache opens sources lazily and reuses the handle when a timeline
// references the same source repeatedly. It caches open files and parsed
// headers only — never decoded audio and never joint analysis.
type sourceCache struct {
	provider SourceProvider
	handles  map[string]*sourceHandle
}

func newSourceCache(provider SourceProvider) *sourceCache {
	return &sourceCache{provider: provider, handles: make(map[string]*sourceHandle)}
}

func (cache *sourceCache) get(id string) (*sourceHandle, error) {
	if handle, ok := cache.handles[id]; ok {
		return handle, nil
	}
	asset, err := cache.provider.GetSourceMeta(id)
	if err != nil {
		return nil, err
	}
	media, err := cache.provider.OpenSourceMedia(id)
	if err != nil {
		return nil, err
	}
	randomAccess, ok := media.(io.ReaderAt)
	if !ok {
		_ = media.Close()
		return nil, domain.E(domain.KindInternal, "source_media_not_seekable", "source_id", "source media handle does not support random access")
	}
	// Verify the stored payload against its recorded digest before any frame
	// is consumed, streaming the bytes so verification stays memory-bounded.
	digest := sha256.New()
	if _, err := io.Copy(digest, media); err != nil {
		_ = media.Close()
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read source media", err)
	}
	if hex.EncodeToString(digest.Sum(nil)) != asset.Digest {
		_ = media.Close()
		return nil, domain.E(domain.KindInternal, "source_integrity_error", "source_id", "stored source digest does not match metadata")
	}
	frames, err := audiocodec.OpenFrameReader(randomAccess, asset.ByteCount, media)
	if err != nil {
		_ = media.Close()
		return nil, domain.Wrap(domain.KindInternal, "source_integrity_error", "source_id", "stored source media failed to parse", err)
	}
	info := frames.Info()
	if info.SampleRate != asset.SampleRate ||
		info.Channels != asset.Channels ||
		info.BitsPerSample != asset.BitsPerSample ||
		info.FrameCount != asset.FrameCount {
		_ = frames.Close()
		return nil, domain.E(domain.KindInternal, "source_integrity_error", "source_id", "stored source media does not match recorded metadata")
	}
	handle := &sourceHandle{asset: asset, frames: frames}
	cache.handles[id] = handle
	return handle, nil
}

func (cache *sourceCache) closeAll() {
	for _, handle := range cache.handles {
		_ = handle.frames.Close()
	}
	cache.handles = make(map[string]*sourceHandle)
}
