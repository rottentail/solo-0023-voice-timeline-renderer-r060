package auditdemo

import (
	"context"
	"fmt"
	"math"
	"time"

	"voice-timeline-renderer/internal/audit"
)

// buildCandidate renders a scenario through the production chain, applies any
// declared post transformation, and describes the resulting candidate. The
// declaration is the producer's statement of what was selected and which
// conditions were adopted; the reviewer rebuilds from it independently.
func buildCandidate(ctx context.Context, runner *Runner, delivery *Delivery, scenario Scenario) (audit.Candidate, []audit.Artifact, error) {
	renderID := "render-" + scenario.Name
	timelineID := "timeline-" + scenario.Name

	// For scenarios that edit inputs, register new source bytes under new IDs
	// so the historical sources are never modified.
	inputs := map[string][]byte{
		baselineSourceA: delivery.SourceBytes[baselineSourceA],
		baselineSourceB: delivery.SourceBytes[baselineSourceB],
	}
	ids := map[string]string{
		baselineSourceA: baselineSourceA,
		baselineSourceB: baselineSourceB,
	}
	if scenario.Transforms.SourceATransform != (scenarioTransform{}) {
		edited, err := editSource(inputs[baselineSourceA], scenario.Transforms.SourceATransform)
		if err != nil {
			return audit.Candidate{}, nil, err
		}
		ids[baselineSourceA] = "src_a_" + scenario.Name
		inputs[ids[baselineSourceA]] = edited
	}
	if scenario.Transforms.SourceBTransform != (scenarioTransform{}) {
		edited, err := editSource(inputs[baselineSourceB], scenario.Transforms.SourceBTransform)
		if err != nil {
			return audit.Candidate{}, nil, err
		}
		ids[baselineSourceB] = "src_b_" + scenario.Name
		inputs[ids[baselineSourceB]] = edited
	}

	spans := make([]struct {
		ID     string
		Start  int64
		End    int64
		GainDB float64
		Cross  int64
	}, 0, len(delivery.Spans))
	for _, span := range delivery.Spans {
		spans = append(spans, struct {
			ID     string
			Start  int64
			End    int64
			GainDB float64
			Cross  int64
		}{ID: ids[span.SourceID], Start: span.SourceStartFrame, End: span.SourceEndFrame, GainDB: span.GainDB, Cross: span.CrossfadeFrames})
	}

	for id, payload := range inputs {
		if err := runner.putSource(ctx, id, payload); err != nil {
			return audit.Candidate{}, nil, err
		}
	}
	plan, err := runner.putTimeline(ctx, timelineID, spans)
	if err != nil {
		return audit.Candidate{}, nil, err
	}
	job, err := runner.putRender(ctx, renderID, plan.ID)
	if err != nil {
		return audit.Candidate{}, nil, err
	}
	if job.State != "completed" || job.Output == nil {
		return audit.Candidate{}, nil, fmt.Errorf("render %s ended in state %s", renderID, job.State)
	}
	receiptBytes, err := runner.getReceipt(ctx, renderID)
	if err != nil {
		return audit.Candidate{}, nil, err
	}
	pcmBytes, err := runner.getOutput(ctx, renderID)
	if err != nil {
		return audit.Candidate{}, nil, err
	}

	// Apply the declared post transformation to the delivered bytes. The
	// transform is a stated condition, not a hidden edit: the record
	// declares it in post_processing, so the reviewer can rebuild it.
	if scenario.Transforms.PostTransform != (scenarioTransform{}) {
		pcmBytes, err = editDelivered(ctx, pcmBytes, scenario.Transforms.PostTransform)
		if err != nil {
			return audit.Candidate{}, nil, err
		}
		// The producer's receipt must describe the bytes it actually
		// delivered. Rewrite the digest and measurements to the edited bytes
		// so the receipt is honest about the artifact; the audit will still
		// detect any disagreement.
		receiptBytes, err = rewriteReceipt(receiptBytes, pcmBytes, scenario.Transforms.PostTransform)
		if err != nil {
			return audit.Candidate{}, nil, err
		}
	}

	receiptEvidence, err := audit.ParseReceiptEvidence(receiptBytes)
	if err != nil {
		return audit.Candidate{}, nil, err
	}

	declared := make([]audit.CandidateSpan, 0, len(plan.Spans))
	for index, span := range plan.Spans {
		declared = append(declared, audit.CandidateSpan{
			Index: index,
			Selection: audit.Selection{
				SourceID:         span.SourceID,
				SourceDigest:     audit.DigestBytes(inputs[span.SourceID]),
				SourceStartFrame: span.SourceStartFrame,
				SourceEndFrame:   span.SourceEndFrame,
				SourceSampleRate: viewRate(inputs[span.SourceID]),
				SourceChannels:   viewChannels(inputs[span.SourceID]),
			},
			Processing: audit.SpanProcessing{
				GainDB:          span.GainDB,
				CrossfadeFrames: span.CrossfadeFrames,
				ResampleMethod:  audit.ResampleLinear,
				ChannelMapping: func() string {
					if span.SourceID == baselineSourceA && viewChannels(inputs[span.SourceID]) == 1 {
						return audit.MappingUnchanged
					}
					if viewChannels(inputs[span.SourceID]) == 2 && plan.OutputChannels == 1 {
						return audit.MappingStereoToMono
					}
					return audit.MappingUnchanged
				}(),
			},
		})
	}

	post := audit.PostProcessing{
		NormalizationTargetDBFS: productionTargetPeakDBFS,
		AppliedGainDB:           receiptEvidence.AppliedGainDB,
		Quantization:            audit.QuantRoundHalfAway,
	}
	if scenario.Transforms.DeclaredQuantization != nil {
		post.Quantization = *scenario.Transforms.DeclaredQuantization
	}
	if scenario.Transforms.DeclaredPolarity != nil {
		post.PolarityInverted = *scenario.Transforms.DeclaredPolarity
	}
	if scenario.Transforms.DeclaredClockRatio != nil {
		post.OutputClockRatio = *scenario.Transforms.DeclaredClockRatio
	}
	if scenario.Transforms.DeclaredPhaseDelay != nil {
		post.PhaseDelayFrames = *scenario.Transforms.DeclaredPhaseDelay
	}

	createdAt := "2026-02-01T10:00:00Z"
	if scenario.LateDelivery {
		// Statement of the producer's own timeline; the record keeps the
		// earlier historical decision untouched.
		createdAt = "2026-02-20T16:30:00Z"
	}

	candidate := audit.Candidate{
		ID:               "candidate-" + scenario.Name,
		Role:             "new_render",
		CreatedAt:        createdAt,
		ProcessorVersion: "voice-timeline-renderer/production",
		TimelineID:       plan.ID,
		SampleRate:       plan.SampleRate,
		OutputChannels:   plan.OutputChannels,
		Post:             post,
		Spans:            declared,
	}

	artifacts := []audit.Artifact{
		{Kind: audit.EvidenceDownloadedPCM, Subject: "output", Extension: "wav", MediaType: "audio/wav",
			Description: "downloaded candidate PCM", Bytes: pcmBytes},
		{Kind: audit.EvidenceTimeline, Subject: plan.ID, Extension: "json", MediaType: "application/json",
			Description: "candidate timeline", Bytes: mustJSON(plan)},
	}
	for id, payload := range inputs {
		artifacts = append(artifacts, audit.Artifact{
			Kind: audit.EvidenceSourceWAV, Subject: id, Extension: "wav", MediaType: "audio/wav",
			Description: "source bytes selected by the candidate", Bytes: payload,
		})
	}
	if !scenario.Transforms.OmitReceipt {
		artifacts = append(artifacts, audit.Artifact{
			Kind: audit.EvidenceReceipt, Subject: "receipt", Extension: "json", MediaType: "application/json",
			Description: "candidate receipt", Bytes: receiptBytes,
		})
	}
	return candidate, artifacts, nil
}

func viewRate(payload []byte) int {
	pcm, err := audit.DecodePCM16Wave(payload)
	if err != nil {
		return 0
	}
	return pcm.SampleRate
}

func viewChannels(payload []byte) int {
	pcm, err := audit.DecodePCM16Wave(payload)
	if err != nil {
		return 0
	}
	return pcm.Channels
}

func editSource(payload []byte, transform scenarioTransform) ([]byte, error) {
	pcm, err := audit.DecodePCM16Wave(payload)
	if err != nil {
		return nil, err
	}
	samples := make([]float64, len(pcm.Samples))
	for index, sample := range pcm.Samples {
		if sample < 0 {
			samples[index] = float64(sample) / 32768
		} else {
			samples[index] = float64(sample) / 32767
		}
	}
	// Use the same transform machinery, without dropping frames or
	// requantizing, so the resulting source remains a valid 16-bit payload.
	samples = applyTransform(samples, pcm.Channels, transform)
	quantized := audit.Quantize16(samples, audit.QuantRoundHalfAway)
	return audit.EncodePCM16Wave(pcm.SampleRate, pcm.Channels, quantized)
}

func editDelivered(ctx context.Context, payload []byte, transform scenarioTransform) ([]byte, error) {
	pcm, err := audit.DecodePCM16Wave(payload)
	if err != nil {
		return nil, err
	}
	samples := make([]float64, len(pcm.Samples))
	for index, sample := range pcm.Samples {
		if sample < 0 {
			samples[index] = float64(sample) / 32768
		} else {
			samples[index] = float64(sample) / 32767
		}
	}
	samples = applyTransform(samples, pcm.Channels, transform)
	quantized := audit.Quantize16(samples, audit.QuantRoundHalfAway)
	return audit.EncodePCM16Wave(pcm.SampleRate, pcm.Channels, quantized)
}

func rewriteReceipt(receiptBytes, pcmBytes []byte, transform scenarioTransform) ([]byte, error) {
	receipt, err := audit.ParseReceiptEvidence(receiptBytes)
	if err != nil {
		return nil, err
	}
	pcm, err := audit.DecodePCM16Wave(pcmBytes)
	if err != nil {
		return nil, err
	}
	observation, err := audit.ObservePCM(context.Background(), "", audit.DigestBytes(pcmBytes), pcm, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	receipt.OutputDigest = audit.DigestBytes(pcmBytes)
	receipt.OutputByteCount = int64(len(pcmBytes))
	receipt.OutputFrameCount = pcm.FrameCount
	receipt.OutputPeakDBFS = observation.PeakDBFS
	receipt.OutputRMSDBFS = observation.RMSDBFS
	if transform.levelDB != 0 {
		receipt.AppliedGainDB += transform.levelDB
	}
	// Re-encode a receipt whose content is consistent with the delivered
	// bytes. The audit independently re-derives every value, so dishonesty
	// in this step would surface as a finding.
	return mustJSON(receiptWire(receipt)), nil
}

type receiptWireShape struct {
	SchemaVersion int    `json:"schema_version"`
	RenderID      string `json:"render_id"`
	TimelineID    string `json:"timeline_id"`
	Output        struct {
		Digest        string  `json:"digest"`
		ByteCount     int64   `json:"byte_count"`
		SampleRate    int     `json:"sample_rate"`
		Channels      int     `json:"channels"`
		BitsPerSample int     `json:"bits_per_sample"`
		FrameCount    int64   `json:"frame_count"`
		DurationMS    int64   `json:"duration_ms"`
		PeakDBFS      float64 `json:"peak_dbfs"`
		RMSDBFS       float64 `json:"rms_dbfs"`
	} `json:"output"`
	Normalization struct {
		RequestedPeakDBFS float64 `json:"requested_peak_dbfs"`
		BeforePeakDBFS    float64 `json:"before_peak_dbfs"`
		AppliedGainDB     float64 `json:"applied_gain_db"`
		OutputPeakDBFS    float64 `json:"output_peak_dbfs"`
	} `json:"normalization"`
	Spans     []audit.ReceiptEvidenceSpan `json:"spans"`
	Warnings  []string                    `json:"warnings"`
	CreatedAt string                      `json:"created_at"`
}

func receiptWire(receipt audit.ReceiptEvidence) receiptWireShape {
	var wire receiptWireShape
	wire.SchemaVersion = 1
	wire.RenderID = receipt.RenderID
	wire.TimelineID = receipt.TimelineID
	wire.Output.Digest = receipt.OutputDigest
	wire.Output.ByteCount = receipt.OutputByteCount
	wire.Output.SampleRate = receipt.OutputSampleRate
	wire.Output.Channels = receipt.OutputChannels
	wire.Output.BitsPerSample = 16
	wire.Output.FrameCount = receipt.OutputFrameCount
	wire.Output.DurationMS = int64(math.Round(float64(receipt.OutputFrameCount) * 1000 / float64(receipt.OutputSampleRate)))
	wire.Output.PeakDBFS = receipt.OutputPeakDBFS
	wire.Output.RMSDBFS = receipt.OutputRMSDBFS
	wire.Normalization.RequestedPeakDBFS = receipt.RequestedPeakDBFS
	wire.Normalization.AppliedGainDB = receipt.AppliedGainDB
	wire.Normalization.OutputPeakDBFS = receipt.OutputPeakDBFS
	wire.Spans = receipt.Spans
	wire.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	return wire
}
