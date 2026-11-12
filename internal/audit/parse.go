package audit

import (
	"encoding/json"
	"fmt"
)

// ParseReceiptEvidence decodes the receipt JSON that the production service
// returns from GET /v1/renders/{id}/receipt. The audit keeps the raw bytes as
// evidence and only reads the fields it reconciles, so a receipt schema
// addition does not break the audit.
func ParseReceiptEvidence(payload []byte) (ReceiptEvidence, error) {
	var wire struct {
		RenderID   string `json:"render_id"`
		TimelineID string `json:"timeline_id"`
		Output     struct {
			Digest     string  `json:"digest"`
			ByteCount  int64   `json:"byte_count"`
			SampleRate int     `json:"sample_rate"`
			Channels   int     `json:"channels"`
			FrameCount int64   `json:"frame_count"`
			PeakDBFS   float64 `json:"peak_dbfs"`
			RMSDBFS    float64 `json:"rms_dbfs"`
		} `json:"output"`
		Normalization struct {
			RequestedPeakDBFS float64 `json:"requested_peak_dbfs"`
			AppliedGainDB     float64 `json:"applied_gain_db"`
		} `json:"normalization"`
		Spans []struct {
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
		} `json:"spans"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return ReceiptEvidence{}, fmt.Errorf("decode receipt evidence: %w", err)
	}
	evidence := ReceiptEvidence{
		Present:           true,
		RenderID:          wire.RenderID,
		TimelineID:        wire.TimelineID,
		OutputDigest:      wire.Output.Digest,
		OutputByteCount:   wire.Output.ByteCount,
		OutputFrameCount:  wire.Output.FrameCount,
		OutputSampleRate:  wire.Output.SampleRate,
		OutputChannels:    wire.Output.Channels,
		OutputPeakDBFS:    wire.Output.PeakDBFS,
		OutputRMSDBFS:     wire.Output.RMSDBFS,
		RequestedPeakDBFS: wire.Normalization.RequestedPeakDBFS,
		AppliedGainDB:     wire.Normalization.AppliedGainDB,
	}
	evidence.Spans = make([]ReceiptEvidenceSpan, 0, len(wire.Spans))
	for _, span := range wire.Spans {
		evidence.Spans = append(evidence.Spans, ReceiptEvidenceSpan{
			Index:            span.Index,
			SourceID:         span.SourceID,
			SourceDigest:     span.SourceDigest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: span.OutputStartFrame,
			OutputEndFrame:   span.OutputEndFrame,
			OutputFrames:     span.OutputFrames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
		})
	}
	return evidence, nil
}
