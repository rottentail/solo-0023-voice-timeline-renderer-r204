# Voice Timeline Renderer

## Service goal

Voice Timeline Renderer is a backend service for assembling narrated PCM audio
into a deterministic mixdown. It gives audio-platform engineers a small HTTP
surface for registering 16-bit WAV sources, composing ordered timeline spans,
and rendering a normalized output along with a machine-readable render
receipt.

The project is intentionally production-only. Automated tests, fixtures, and a
test command are deferred to the downstream task stage, which will add focused
unit, integration, and end-to-end verification.

## Users

- Audio-platform engineers who need a self-hosted assembly stage.
- Audio editing tooling that submits WAV sources and timeline operations.
- Quality engineers who replay a render and compare the receipt with the output.

## Project shape and stack

- Shape: backend.
- Language: Go.
- Runtime: Go bundled HTTP packages.
- Persistence: a local data directory containing metadata JSON and media files.
- External services: none.

## Core entities

### Source asset

A source asset is an immutable uploaded PCM WAV. Its identity, SHA-256 digest,
frame count, sample rate, channel count, peak amplitude, RMS amplitude, and duration are
recorded after decoding. A source is never updated in place.

Invariants:

- The payload must be a RIFF/WAVE file with 16-bit integer PCM samples.
- Only mono and stereo layouts are accepted.
- The digest and signal measurements describe the exact bytes stored.
- Decoded payloads are bounded by configured byte and duration limits.

### Assembly timeline

A timeline is an ordered set of spans over existing source assets. A span
selects a half-open source frame interval, a gain in decibels, and an optional
crossfade with the preceding span.

Invariants:

- A timeline has at least one span.
- Every referenced source exists.
- Source intervals are non-empty and remain inside the source frame count.
- The first span has no crossfade.
- A crossfade cannot exceed the shorter neighboring span.
- Output channel count is explicit and independent from an input source's
  channel count.

### Render job

A render job is the durable record of one timeline execution.

State machine:

`queued -> rendering -> finalizing -> completed`

Any active state may transition to `failed`. `completed` and `failed` are
terminal. A failed job stores a stable error code and message. A completed job
stores the output file, output metadata, and immutable render receipt.

### Render receipt

A receipt records source identifiers, span placement, gain adjustments, peak
and RMS measurements, final normalization gain, frame count, output digest,
per-joint transition decisions, and non-fatal warnings. It is written only
after the output bytes have been atomically committed.

Each crossfade joint produces a transition entry recording the measured
overlap correlation, the fade curve family, the curve exponent actually used,
and an optional conflict marker. When neighboring content is anti-correlated
enough that no amplitude law can keep the seam flat, the joint keeps the
equal-power floor and is flagged with a `cancellation` conflict instead of
flipping polarity or shifting placement to fake a flat seam.

## Workflows

### 1. Register audio source

Entry point: `PUT /v1/sources/{source_id}`.

1. The HTTP boundary enforces request size and base64 decoding.
2. The service validates identity fields and decodes the WAV structure.
3. The codec layer verifies PCM format, frame alignment, and signal bounds.
4. Signal measurements are computed from the decoded samples.
5. Storage writes the media bytes and metadata using an atomic commit.

Success returns source metadata. Failure returns a stable validation error and
leaves no visible partial source.

### 2. Compose assembly timeline

Entry point: `PUT /v1/timelines/{timeline_id}`.

1. The HTTP boundary decodes and sizes the span list.
2. Domain validation checks ranges, gain limits, crossfade limits, and temporal
   ordering.
3. The service resolves every source reference from storage.
4. A timeline record is persisted once all spans are valid.

Success returns a ready timeline. Failure reports the first stable validation
issue without persisting a partial timeline.

### 3. Render timeline mixdown

Entry point: `PUT /v1/renders/{render_id}` with a `timeline_id` body field.

1. The service creates a durable `queued` job.
2. The renderer moves to `rendering` and plans span placement from source
   metadata.
3. Each crossfade joint is analyzed by streaming the overlap in bounded
   blocks: the zero-lag correlation between the outgoing tail and the
   incoming head decides the fade law for that joint. Correlated content
   moves the law toward constant-amplitude weights, uncorrelated content
   keeps equal-power weights, and strongly anti-correlated content keeps the
   equal-power floor plus an explicit cancellation conflict.
4. Each source interval is read through bounded frame windows, resampled to
   the timeline sample rate, mapped to the requested channel layout,
   gain-adjusted, and mixed under its fade envelope. A span only contributes
   inside its recorded output range, even when its incoming and outgoing
   fades overlap.
5. The finalizing stage streams the mix in blocks, applies peak normalization
   to the requested target while quantizing, and writes the WAV and receipt
   through temporary files, renaming them into place atomically.
6. The job moves to `completed`. Any error records a failed terminal state and
   removes unpublished temporary output.

Success is observable through the job resource, the downloaded WAV, and the
receipt. Failure is observable through the terminal job error code.

## Crossfade law

For a joint with measured overlap correlation `rho`, the fade weights are

```
w_in(u)  = sin^gamma(u * pi / 2)
w_out(u) = cos^gamma(u * pi / 2)
gamma    = 1 + log2(1 + rho)   for rho > 0.05
gamma    = 1                   otherwise
```

With `w = (1/sqrt(2))^gamma`, the seam-center power `2 w^2 (1 + rho)` equals
the neighboring solo power for any `rho >= 0`, so `gamma = 1` is the classic
equal-power law for uncorrelated content and `gamma = 2` the
constant-amplitude law for identical content. Negative correlations would
need weights above the equal-power envelope to stay flat, which is impossible
to honor as `rho` approaches -1; those joints keep `gamma = 1` and record a
`cancellation` conflict on the receipt.

## Resource bounds

Analysis and rendering are streaming pipelines. Joint analysis keeps only
running statistics per joint, sources are read through frame windows, the mix
is produced in bounded blocks (one pass for the peak, one for the encode),
and the output is quantized straight into its staged file. A 64-span timeline
at the configured frame limit renders within a 128 MiB whole-process RSS
budget; the `crossfade-quality` workflow check measures and enforces this.

## HTTP interface

- `GET /healthz`
- `PUT /v1/sources/{source_id}`
- `GET /v1/sources/{id}`
- `PUT /v1/timelines/{timeline_id}`
- `GET /v1/timelines/{id}`
- `PUT /v1/renders/{render_id}`
- `GET /v1/renders/{id}`
- `GET /v1/renders/{id}/receipt`
- `GET /v1/renders/{id}/output`

Errors use a JSON envelope:

```json
{
  "error": {
    "code": "invalid_source_interval",
    "message": "source interval ends beyond the source frame count",
    "field": "spans[0].source_end_frame"
  }
}
```

## Modules

| Module | Responsibility |
|---|---|
| `httpapi` | HTTP routing, request limits, JSON encoding, media responses |
| `service` | Workflow orchestration and job lifecycle |
| `domain` | Entities, validation rules, stable errors, state transitions |
| `audiocodec` | WAV parsing and encoding, windowed frame reads, resampling, channel mapping, signal metrics |
| `render` | Span planning, joint analysis, crossfade mixing, normalization, receipt construction |
| `storage` | Atomic source, timeline, job, output, and receipt persistence |

## Engineering mechanisms

- State transitions: render jobs persist every active and terminal transition.
- Error propagation: domain errors retain stable codes through HTTP responses.
- Resource lifecycle: temporary files are cleaned after failed publication.
- Concurrency: storage guards indexes and registry updates; unique IDs and
  atomic renames prevent duplicate or half-visible records.
- Validation boundaries: request size, PCM layout, timeline bounds, and gain
  ranges are checked before allocation-heavy work.

## Validation plan

The runtime manifest exposes one bounded production smoke path per workflow:

- Register a generated WAV, read it back, and confirm a malformed payload fails.
- Compose a two-span timeline, read it back, and confirm an invalid crossfade
  fails.
- Render a crossfaded timeline, download the output, decode it, and match core
  receipt measurements.
- Render independently synthesized correlated, uncorrelated, and
  anti-correlated joints; confirm the seam stays within 1 dB of the expected
  level for the first two, that the cancellation case is reported on the
  receipt without polarity or placement changes, that double-sided fades keep
  every source's range consistent with the project, and that a 64-span
  maximum timeline renders within the 128 MiB process RSS budget with
  byte-identical re-renders.

These checks use the real HTTP entry points and local storage. They are not a
replacement for the test suite that the later task stage will add.
