# Audio change review procedure

This is the maintenance procedure for deciding whether an audio-rendering
change may be released as-is, may be accepted as a compatible replacement, or
must block release. It applies to changes in the processing chain that
produces the mixdown delivered by `PUT /v1/renders/{render_id}` and downloaded
from `GET /v1/renders/{render_id}/output`.

The procedure is implemented by `internal/audit` and rehearsed by
`cmd/audioaudit`. It does not change the production algorithm: it reads the
production render, the sources it selected, the receipt it published, and the
downloaded PCM, and records an auditable conclusion.

## The conflict this procedure resolves

Two obligations can disagree:

1. **The historical byte promise.** A delivery was accepted earlier. Its exact
   output digest is a promise that downstream consumers may rely on.
2. **The new signal-quality target.** A newer production run is required to
   meet a peak target, a loudness-continuity bound, and a format contract.

A newer delivery cannot satisfy both. The procedure does not resolve the
conflict by preferring one side. It resolves it by **attribution**:

- If the bytes are identical to the accepted delivery, the result is
  `release_pass`.
- If the bytes differ, the difference must be attributed to a named class with
  a bounded magnitude. If every attributable difference is in a tolerated
  class, and all signal-quality targets hold, the result is
  `compatible_accept`.
- If any difference exceeds its class's bound, or cannot be attributed at all,
  the result is `block_release`.
- If the record cannot be independently rebuilt, the result is
  `insufficient_evidence`. This is never treated as a pass.
- Precedence: a `block_release` finding outranks `insufficient_evidence`. A
  definitively identified defect must not be retried as a paperwork problem.

Two failure modes are explicitly prohibited:

- **Do not waive every small difference.** A one-LSB waveform change is
  tolerated only because it is *requantization* and the class is bounded.
  A small change that survives alignment and is not quantization is
  `waveform_residual_over_tolerance` and blocks release however small it is.
- **Do not fail every legal numeric difference.** One-LSB requantization
  noise, a one-frame rounding difference at a rate boundary, and a phase
  shift declared and reconstructed from the record are all legal outcomes
  under this table. Byte identity is not the only acceptable outcome.

## Evidence an independent reviewer must be able to reproduce

Every record holds, per candidate:

- **Source selection** — the source identifier, the SHA-256 of the exact
  source bytes, the half-open frame interval, and the source's rate and channel
  count.
- **Adopted processing conditions** — gain, crossfade, resample method,
  channel mapping, phase delay, polarity, output clock ratio, and the
  quantization convention. An absent condition is stated explicitly, so
  "not applied" is distinguishable from "not recorded".
- **Downloaded PCM** — the exact bytes returned by the output route.
- **Receipt** — the exact bytes returned by the receipt route.

Every artifact is stored content-addressed by SHA-256 and re-verified on read.
A byte that no longer matches its digest is reported as evidence drift, not
used.

## The four outcomes

| Outcome | Meaning |
|---|---|
| `release_pass` | Downloaded bytes equal the accepted historical delivery and all signal-quality targets hold. |
| `compatible_accept` | Bytes differ, but every difference is attributed to a tolerated class and all targets hold. |
| `block_release` | An attributable difference exceeds its bound, or is unclassified. |
| `insufficient_evidence` | The record cannot be rebuilt: a receipt is missing, the declared selection or conditions do not reproduce the download, or the artifacts contradict each other. When any blocking finding is also present, `block_release` takes precedence. |

## The rule table

`internal/audit/evaluate.go` contains the authoritative table. It is the only
place a finding class is assigned a decision level. An unlisted class blocks
release and is marked `unclassified`, so a new difference cannot be waived by
omission.

### Insufficient evidence (never a pass)

| Class | Meaning |
|---|---|
| `receipt_missing` | No receipt, so the download cannot be reconciled with a declared result. |
| `selection_source_missing` | The record does not carry the source bytes the span selected. |
| `reconstruction_not_possible` | The declared selection or conditions cannot be rebuilt. |
| `declared_processing_does_not_explain_download` | The independent rebuild does not reproduce the download. |
| `declared_format_does_not_match_download` | The declared format disagrees with the artifact. |
| `reconstruction_frame_count_mismatch` | The declared spans do not predict the delivered frame count. |
| `reconstruction_missing` | No reconstruction was recorded. |
| `evidence_incomplete` | Candidate, baseline, or observation facts are missing. |
| `waveform_not_comparable` | The artifacts cannot be aligned, so no cause can be attributed. |
| `timebase_not_measurable` | The payload is too short to measure drift, so timebase stability is unverified. |

### Blocking (release fails)

| Class | Cause class |
|---|---|
| `receipt_digest_mismatch`, `receipt_frame_count_mismatch`, `receipt_peak_disagrees_with_download`, `receipt_rms_disagrees_with_download`, `receipt_spans_disagree_with_selection`, `receipt_normalization_target_mismatch` | receipt integrity |
| `selection_source_rate_mismatch`, `selection_source_channels_mismatch` | declaration integrity |
| `sample_rate_mismatch`, `channel_layout_mismatch`, `bit_width_mismatch`, `frame_count_mismatch` | format and timing |
| `phase_offset`, `polarity_inversion` | phase |
| `resampling_timebase_change` | resampling |
| `peak_target_missed`, `loudness_drift`, `clipping`, `silent_output` | level |
| `waveform_residual_over_tolerance` | waveform |

### Tolerated (bounded, attributable)

| Class | Bound |
|---|---|
| `quantization_noise` | Aligned residual must stay at or below the record's quantization tolerance (in 16-bit LSB). |
| `digitally_identical_waveform` | Aligned residual is zero. |
| `frame_count_within_duration_tolerance` | Frame-count delta must stay at or below the record's duration tolerance. |

## Separating adjacent causes

The rule set must not confuse one cause with its neighbour. These three
measurements are the mechanism, and each maps to a distinct class:

| Measurement | Distinguishes | Class |
|---|---|---|
| Constant lag of the whole waveform | true alignment change | `phase_offset` |
| Lag measured in several windows across the duration; the spread between them | same-rate timebase change | `resampling_timebase_change` |
| Residual after alignment, at the best lag, with the best-fit scalar gain removed | requantization vs level vs waveform content | `quantization_noise`, level classes, `waveform_residual_over_tolerance` |

The rule is:

- A constant lag is a **phase** cause. It blocks release even when it is equal
  in magnitude to a legal rounding difference.
- A lag that *grows across the duration* is a **resampling** cause, even when
  the declared sample rate did not change. Reporting it as `phase_offset` would
  name the wrong cause, so only `resampling_timebase_change` is emitted.
- A residual that survives alignment inside the quantization tolerance is
  **requantization**. A residual that survives alignment outside the tolerance
  is a **waveform** cause. A residual explained by one fitted scalar gain is a
  **level** cause.
- A timebase change makes every sample drift against the reference, so its
  residual is not reported as quantization or waveform content. The timebase
  finding carries the block.

Exports the procedure uses and their thresholds:

| Constant | Value | Meaning |
|---|---|---|
| `PhaseToleranceSamples` | 0.5 | Largest constant delay that counts as aligned. |
| `LagDriftToleranceSamples` | 0.25 | Largest lag change across the duration that counts as one timebase. |
| `ActiveSampleThreshold` | 1/32767 | Samples quieter than one LSB count as silence. |
| `maxAnalysisFrames` | 65536 | Longest common prefix used for lag and residual measurement. |
| `segmentFrames` × `segmentPositions` | 1024 × 5 | Below two usable windows, drift is reported as unmeasurable rather than zero. |

Changing any of these, or the rule table, requires a **new rule-set
identifier**. Verdicts are stored per candidate and per rule set, so an older
conclusion remains readable and is never overwritten.

## Immutability and late deliveries

A third-party re-render may arrive after the historical delivery was accepted,
and may even be *produced* before the audit. The procedure keeps both facts:

- The record header (`record.json`) is written once. Fidelity targets, the rule
  set, and the historical digest are never recomputed.
- Candidates are append-only. Submitting an existing candidate identifier is
  refused, even with identical content, so later bytes never erase earlier
  bites.
- Observations, reconstructions, verdicts, and reports are write-once per
  candidate and rule set. `Review` returns the recorded verdict instead of
  recomputing it.
- Decisions are an append-only sequence. The historical acceptance stays at
  sequence 1 after a later candidate is decided.
- Every mutation appends a hash-chained event. `VerifyChain` recomputes the
  chain, so reordering or rewriting an earlier event is detectable.
- Evidence is re-hashed on every read and on an explicit recheck, so the
  historical delivery's bytes can be shown intact after later work.

Arrival order therefore does not decide anything. The record decides.

## Reconstruction from the record

The reviewer's rebuild is independent of the production renderer:

1. Slice the declared source frames from the record's own copy of the source
   bytes.
2. Resample with the declared method.
3. Map channels with the declared mapping.
4. Apply the declared gain.
5. Place spans at the declared output positions and mix the declared
   crossfades.
6. Apply the declared level, delay, polarity, and clock-ratio conditions.
7. Quantize with the declared convention.

The result is compared with the downloaded PCM and with the receipt. A
reconstruction that reproduces the download under tolerance, whose receipt
digest, frame count, peak, RMS, span placement, and normalization target all
agree, proves the record explains the delivery. A reconstruction that does not
is `insufficient_evidence`, not a release failure and not a pass.

## Running the rehearsal

```bash
go run ./cmd/audioaudit -out ./var/audio-audit
```

The command runs the real production chain through its HTTP entry points,
records the historical delivery as the baseline of eleven independent records,
submits one candidate per record, and audits it. Each record states the
intended cause, the required outcome, and the finding class that must prove the
cause. A scenario fails the run if either the outcome or the proving class is
wrong.

Re-running against the same directory re-reads recorded conclusions; it does
not recompute or overwrite them.

## Record layout

```text
<record>/
  record.json                  captured targets and rule set, written once
  events.jsonl                 append-only, hash-chained audit log
  decisions/NNNN-<candidate>.json   append-only decision sequence
  verdicts/<candidate>.<ruleset>.json  write-once per candidate and rule set
  reports/<candidate>.<ruleset>.md     write-once human-readable review
  candidates/<candidate>/
    candidate.json             declared selection and processing conditions
    observation.json           measurements of the downloaded PCM
    reconstruction.json        independent rebuild result
    evidence/<kind>/<subject>/<digest>.<ext>   content-addressed artifacts
```

## Scope boundary

This procedure does not alter the production algorithm, does not add a
processing stage, and does not change any HTTP response. It adds a review
package, a rehearsal command, and this procedure. The production modules
(`audiocodec`, `render`, `service`, `storage`, `domain`, `httpapi`,
`checkflow`) are unchanged.
