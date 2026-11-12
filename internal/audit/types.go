package audit

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RecordSchemaVersion is the stable on-disk schema for an audit record folder.
const RecordSchemaVersion = "audio-change-audit/v1"

// RuleSetID is the version of the maintenance rule table applied by this
// package. A new rule version must get a new identifier and must never
// overwrite verdicts produced under an older one.
const RuleSetID = "audio-change-rules/v1"

// Stable outcome codes. The reviewer-facing report maps them to plain
// language. They are stored verbatim and are safe to branch on.
const (
	OutcomeReleasePass          = "release_pass"
	OutcomeCompatibleAccept     = "compatible_accept"
	OutcomeBlockRelease         = "block_release"
	OutcomeInsufficientEvidence = "insufficient_evidence"
	// OutcomeHistoricalAccept records a decision made before this audit
	// existed. It is never produced by the current rule engine; it exists so
	// the historical acceptance remains readable alongside newer verdicts.
	OutcomeHistoricalAccept = "historical_accept"
)

// Stable evidence kinds.
const (
	EvidenceSourceWAV     = "source_wav"
	EvidenceDownloadedPCM = "downloaded_pcm"
	EvidenceReceipt       = "receipt_json"
	EvidenceTimeline      = "timeline_json"
)

// Declared processing conditions. A reviewer can rebuild the candidate only
// when every one of these names a supported, deterministic method.
const (
	ResampleLinear          = "linear"
	ResampleNearest         = "nearest_neighbour"
	MappingUnchanged        = "unchanged"
	MappingMonoToStereo     = "mono_to_stereo"
	MappingStereoToMono     = "stereo_to_mono"
	QuantRoundHalfAway      = "round_half_away"
	QuantTruncateTowardZero = "truncate_toward_zero"
	QuantBit14Truncate      = "bit14_truncate"
)

// FidelityTargets captures both halves of the conflicting obligation:
//
//   - HistoricalOutputDigest is the byte-level promise made by the delivery
//     that was accepted earlier. It cannot be recomputed from a newer render.
//   - The numeric fields are the signal-quality targets a newer production
//     run must meet.
//
// The maintenance rules resolve the conflict explicitly: departure from the
// historical bytes is accepted only when every observed difference belongs to
// a declared tolerated class. The targets are captured once, at record
// creation, and copied verbatim by every later artifact.
type FidelityTargets struct {
	SampleRate               int     `json:"sample_rate"`
	Channels                 int     `json:"channels"`
	BitsPerSample            int     `json:"bits_per_sample"`
	TargetPeakDBFS           float64 `json:"target_peak_dbfs"`
	PeakToleranceDB          float64 `json:"peak_tolerance_db"`
	LoudnessToleranceDB      float64 `json:"loudness_tolerance_db"`
	QuantizationToleranceLSB float64 `json:"quantization_tolerance_lsb"`
	DurationToleranceMS      float64 `json:"duration_tolerance_ms"`
	HistoricalOutputDigest   string  `json:"historical_output_digest"`
	HistoricalOutputBytes    int64   `json:"historical_output_bytes"`
	HistoricalReceiptDigest  string  `json:"historical_receipt_digest,omitempty"`
}

// Evidence is one immutable, reviewable artifact attached to a change record.
// The digest is the reviewable byte identity; the path is only a locator and
// is re-verified against the digest at read time.
type Evidence struct {
	Kind         string `json:"kind"`
	Subject      string `json:"subject,omitempty"`
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
	ByteCount    int64  `json:"byte_count"`
	MediaType    string `json:"media_type,omitempty"`
	Description  string `json:"description,omitempty"`
}

// Artifact is the content a caller submits for evidence storage. The store
// computes the digest and never lets a caller choose or overwrite one.
type Artifact struct {
	Kind        string
	Subject     string
	Extension   string
	MediaType   string
	Description string
	Bytes       []byte
}

// Selection records exactly which source bytes and source frame interval a
// candidate rendering selected. It is the primary input an independent
// reviewer uses to rebuild the choice.
type Selection struct {
	SourceID         string `json:"source_id"`
	SourceDigest     string `json:"source_digest"`
	SourceStartFrame int64  `json:"source_start_frame"`
	SourceEndFrame   int64  `json:"source_end_frame"`
	SourceSampleRate int    `json:"source_sample_rate"`
	SourceChannels   int    `json:"source_channels"`
}

// SpanProcessing records the processing conditions adopted for one span.
type SpanProcessing struct {
	GainDB          float64 `json:"gain_db"`
	CrossfadeFrames int64   `json:"crossfade_frames"`
	ResampleMethod  string  `json:"resample_method"`
	ChannelMapping  string  `json:"channel_mapping"`
}

// CandidateSpan binds one selection to the processing conditions applied to it.
type CandidateSpan struct {
	Index      int            `json:"index"`
	Selection  Selection      `json:"selection"`
	Processing SpanProcessing `json:"processing"`
}

// PostProcessing records the output-level conditions that apply after the
// spans have been mixed.
type PostProcessing struct {
	NormalizationTargetDBFS float64 `json:"normalization_target_dbfs"`
	AppliedGainDB           float64 `json:"applied_gain_db"`
	PhaseDelayFrames        float64 `json:"phase_delay_frames"`
	PolarityInverted        bool    `json:"polarity_inverted"`
	// OutputClockRatio stretches the output timebase around the midpoint.
	// It exists so a candidate whose resampler ran at a slightly wrong rate
	// can declare that condition and be reconstructed rather than guessed at.
	OutputClockRatio float64 `json:"output_clock_ratio"`
	Quantization     string  `json:"quantization"`
}

// Candidate is one delivery attempt being audited. Candidates are append-only:
// re-auditing adds a new candidate and leaves earlier evidence and verdicts
// untouched. Arrival order is not assumed; CreatedAt records when the producer
// says the attempt was made, which may precede the audit itself.
type Candidate struct {
	ID               string          `json:"id"`
	Role             string          `json:"role"`
	CreatedAt        string          `json:"created_at"`
	ProcessorVersion string          `json:"processor_version"`
	TimelineID       string          `json:"timeline_id"`
	SampleRate       int             `json:"sample_rate"`
	OutputChannels   int             `json:"output_channels"`
	Post             PostProcessing  `json:"post_processing"`
	Spans            []CandidateSpan `json:"spans"`
	Evidence         []Evidence      `json:"evidence"`
}

// Observation is what a reviewer measured directly from downloaded PCM.
// Observations are stored as facts; the verdict derives from them under a
// versioned rule set and never edits them.
type Observation struct {
	CandidateID        string  `json:"candidate_id"`
	MeasuredAt         string  `json:"measured_at"`
	PCMDigest          string  `json:"pcm_digest"`
	PCMBytes           int64   `json:"pcm_bytes"`
	FrameCount         int64   `json:"frame_count"`
	SampleRate         int     `json:"sample_rate"`
	Channels           int     `json:"channels"`
	BitsPerSample      int     `json:"bits_per_sample"`
	PeakDBFS           float64 `json:"peak_dbfs"`
	RMSDBFS            float64 `json:"rms_dbfs"`
	PeakSampleAbs      float64 `json:"peak_sample_abs"`
	PeakSampleFrame    int64   `json:"peak_sample_frame"`
	ClippedSampleCount int64   `json:"clipped_sample_count"`
	ActiveFrameCount   int64   `json:"active_frame_count"`
	MeanAbsSample      float64 `json:"mean_abs_sample"`
	DurationMS         float64 `json:"duration_ms"`
}

// StructuralComparison reports format and timing differences without any
// resampling, so a wrong converter is attributable before waveform comparison.
type StructuralComparison struct {
	RateMatches         bool    `json:"rate_matches"`
	ChannelsMatch       bool    `json:"channels_match"`
	BitsMatch           bool    `json:"bits_match"`
	FramesMatch         bool    `json:"frame_count_matches"`
	BaselineSampleRate  int     `json:"baseline_sample_rate"`
	CandidateSampleRate int     `json:"candidate_sample_rate"`
	BaselineChannels    int     `json:"baseline_channels"`
	CandidateChannels   int     `json:"candidate_channels"`
	BaselineBits        int     `json:"baseline_bits_per_sample"`
	CandidateBits       int     `json:"candidate_bits_per_sample"`
	BaselineFrames      int64   `json:"baseline_frames"`
	CandidateFrames     int64   `json:"candidate_frames"`
	DurationDeltaMS     float64 `json:"duration_delta_ms"`
}

// WaveformComparison separates adjacent causes. Segment lags expose phase
// shifts (constant lag) from resampling drift (lag that changes over time),
// and only an aligned residual distinguishes quantization from a real signal
// change.
type WaveformComparison struct {
	Comparable                 bool    `json:"comparable"`
	Reason                     string  `json:"reason,omitempty"`
	FrameCountsDiffer          bool    `json:"frame_counts_differ"`
	SegmentLagsAvailable       bool    `json:"segment_lags_available"`
	FullLagSamples             float64 `json:"full_lag_samples"`
	FullCorrelation            float64 `json:"full_correlation"`
	StartLagSamples            float64 `json:"start_lag_samples"`
	MidLagSamples              float64 `json:"mid_lag_samples"`
	EndLagSamples              float64 `json:"end_lag_samples"`
	LagDriftSamples            float64 `json:"lag_drift_samples"`
	PolarityInverted           bool    `json:"polarity_inverted"`
	FittedGainDB               float64 `json:"fitted_gain_db"`
	MaxResidualLSB             float64 `json:"max_residual_lsb"`
	MeanResidualLSB            float64 `json:"mean_residual_lsb"`
	ResidualComparable         bool    `json:"residual_comparable"`
	ResidualOverToleranceCount int64   `json:"residual_over_tolerance_count"`
	ResidualSampleCount        int64   `json:"residual_sample_count"`
	QuantizationTolerance      float64 `json:"quantization_tolerance_lsb"`
	RMSDeltaDB                 float64 `json:"rms_delta_db"`
	PeakDeltaDB                float64 `json:"peak_delta_db"`
}

// Finding is one independently attributable deviation class.
type Finding struct {
	Class      string   `json:"class"`
	Attributes []string `json:"attributes,omitempty"`
	Detail     string   `json:"detail"`
	Magnitude  float64  `json:"magnitude"`
	Tolerated  bool     `json:"tolerated"`
	Blocking   bool     `json:"blocking"`
}

// CheckResult reports one deterministically evaluated rule.
type CheckResult struct {
	Name    string  `json:"name"`
	Passed  bool    `json:"passed"`
	Value   float64 `json:"value"`
	Limit   float64 `json:"limit"`
	Unit    string  `json:"unit"`
	Details string  `json:"details,omitempty"`
}

// Verdict is the auditable conclusion for one candidate under one rule set.
// Verdicts are immutable once written: a later rule set or a later candidate
// produces a new verdict file, never a replacement.
type Verdict struct {
	CandidateID             string               `json:"candidate_id"`
	BaselineCandidateID     string               `json:"baseline_candidate"`
	RuleSetID               string               `json:"rule_set_id"`
	Outcome                 string               `json:"outcome"`
	ByteIdenticalToBaseline bool                 `json:"byte_identical_to_baseline"`
	DownloadedDigest        string               `json:"downloaded_digest"`
	DownloadedFrames        int64                `json:"downloaded_frames"`
	ReconstructionAvailable bool                 `json:"reconstruction_available"`
	ReconstructionMatches   bool                 `json:"reconstruction_matches_download"`
	ReconstructionFrames    int64                `json:"reconstruction_frames"`
	Reasons                 []string             `json:"reasons"`
	Checks                  []CheckResult        `json:"checks"`
	Findings                []Finding            `json:"findings"`
	Structural              StructuralComparison `json:"structural"`
	Waveform                WaveformComparison   `json:"waveform"`
	DecidedAt               string               `json:"decided_at"`
}

// ReceiptAudit is the reviewer's reconstruction of the receipt from the
// downloaded PCM and the declared selection and processing conditions.
type ReceiptAudit struct {
	Present              bool   `json:"present"`
	DigestMatchesPCM     bool   `json:"digest_matches_pcm"`
	FrameCountMatches    bool   `json:"frame_count_matches"`
	PeakMatches          bool   `json:"peak_matches"`
	RMSMatches           bool   `json:"rms_matches"`
	SpansMatchSelection  bool   `json:"spans_match_selection"`
	NormalizationMatches bool   `json:"normalization_matches"`
	Detail               string `json:"detail,omitempty"`
}

// Reconstruction reports the independent rebuild of the choice from the
// source selection, the adopted processing conditions, the downloaded PCM, and
// the receipt.
type Reconstruction struct {
	CandidateID         string       `json:"candidate_id"`
	RuleSetID           string       `json:"rule_set_id"`
	BuiltAt             string       `json:"built_at"`
	Available           bool         `json:"available"`
	Reason              string       `json:"reason,omitempty"`
	ExpectedFrames      int64        `json:"expected_frames"`
	DownloadedFrames    int64        `json:"downloaded_frames"`
	ReferencePCMDigest  string       `json:"reference_pcm_digest"`
	DownloadedPCMDigest string       `json:"downloaded_pcm_digest"`
	FramesMatch         bool         `json:"frame_count_matches"`
	ResidualAvailable   bool         `json:"residual_available"`
	MatchesDownloaded   bool         `json:"reference_matches_downloaded_under_tolerance"`
	MaxResidualLSB      float64      `json:"max_residual_lsb"`
	MeanResidualLSB     float64      `json:"mean_residual_lsb"`
	Findings            []Finding    `json:"findings"`
	Receipt             ReceiptAudit `json:"receipt"`
	Notes               []string     `json:"notes,omitempty"`
}

// Decision is one recorded disposition. Decisions are append-only. A later
// third-party candidate produces an additional decision; the earlier decision
// (including the historical delivery) remains verbatim in the record.
type Decision struct {
	Sequence    int    `json:"sequence"`
	DecidedAt   string `json:"decided_at"`
	Actor       string `json:"actor"`
	CandidateID string `json:"candidate_id"`
	RuleSetID   string `json:"rule_set_id"`
	Outcome     string `json:"outcome"`
	Rationale   string `json:"rationale"`
	Historical  bool   `json:"historical"`
}

// AuditEvent is an append-only, hash-chained log entry. A later review can
// confirm that nothing earlier was rewritten by recomputing the chain.
type AuditEvent struct {
	Sequence      int    `json:"sequence"`
	OccurredAt    string `json:"occurred_at"`
	Actor         string `json:"actor"`
	Action        string `json:"action"`
	Subject       string `json:"subject"`
	Detail        string `json:"detail"`
	PayloadDigest string `json:"payload_digest"`
	PreviousHash  string `json:"previous_hash"`
	Hash          string `json:"hash"`
}

// ChangeRecord is the machine-readable top of an audit folder. It is written
// once at creation: fidelity targets, rule set and identity never change.
// Later state lives in the append-only log and in per-candidate files.
type ChangeRecord struct {
	SchemaVersion     string          `json:"schema_version"`
	RecordID          string          `json:"record_id"`
	Title             string          `json:"title"`
	CreatedAt         string          `json:"created_at"`
	FidelityTargets   FidelityTargets `json:"fidelity_targets"`
	RuleSetID         string          `json:"rule_set_id"`
	BaselineCandidate string          `json:"baseline_candidate"`
	Actor             string          `json:"actor"`
}

// ReceiptEvidence is the receipt content a candidate supplied, flattened to
// the fields the reviewer can reconcile against downloaded PCM. It is a
// parsed view; the authoritative bytes live under Evidence.
type ReceiptEvidence struct {
	Present           bool                  `json:"present"`
	RenderID          string                `json:"render_id"`
	TimelineID        string                `json:"timeline_id"`
	OutputDigest      string                `json:"output_digest"`
	OutputByteCount   int64                 `json:"output_byte_count"`
	OutputFrameCount  int64                 `json:"output_frame_count"`
	OutputSampleRate  int                   `json:"output_sample_rate"`
	OutputChannels    int                   `json:"output_channels"`
	OutputPeakDBFS    float64               `json:"output_peak_dbfs"`
	OutputRMSDBFS     float64               `json:"output_rms_dbfs"`
	RequestedPeakDBFS float64               `json:"requested_peak_dbfs"`
	AppliedGainDB     float64               `json:"applied_gain_db"`
	Spans             []ReceiptEvidenceSpan `json:"spans"`
}

// ReceiptEvidenceSpan is the receipt's per-span placement record.
type ReceiptEvidenceSpan struct {
	Index            int     `json:"index"`
	SourceID         string  `json:"source_id"`
	SourceDigest     string  `json:"source_digest"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	OutputStartFrame int64   `json:"output_start_frame"`
	OutputEndFrame   int64   `json:"output_end_frame"`
	OutputFrames     int64   `json:"output_frames"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
	GainDB           float64 `json:"gain_db"`
}

// ErrEvidenceDrift indicates a stored artifact no longer matches its digest.
var ErrEvidenceDrift = errors.New("stored evidence does not match its recorded digest")

// ErrDuplicateCandidate indicates an attempt to rewrite an existing candidate.
var ErrDuplicateCandidate = errors.New("candidate already exists and cannot be rewritten")

// ErrImmutableResult indicates an attempt to overwrite an existing verdict,
// observation, reconstruction, or report with different content.
var ErrImmutableResult = errors.New("audit result already exists with different content")

func digestBytes(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// DigestBytes exposes the same SHA-256 convention to callers that need to
// state a digest before submitting bytes to the store.
func DigestBytes(payload []byte) string {
	return digestBytes(payload)
}

func digestFile(path string) (string, int64, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer handle.Close()
	hasher := sha256.New()
	count, err := io.Copy(hasher, handle)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), count, nil
}

func eventHash(sequence int, occurredAt, actor, action, subject, detail, payloadDigest, previous string) string {
	payload := fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s|%s", sequence, occurredAt, actor, action, subject, detail, payloadDigest, previous)
	sum := sha512.Sum512([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func sortStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func joinDetails(parts ...string) string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	return strings.Join(filtered, "; ")
}

func readContextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func ensureDir(path string) error {
	return os.MkdirAll(path, 0o750)
}

func writeExclusive(path string, payload []byte, mode os.FileMode) error {
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := handle.Write(payload); err != nil {
		_ = handle.Close()
		return err
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return err
	}
	return handle.Close()
}

func writeTempThenRename(directory, name string, payload []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(directory, ".audit-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		_ = os.Remove(tempName)
	}()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(payload); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, filepath.Join(directory, name))
}

func marshalIndented(value any) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}
