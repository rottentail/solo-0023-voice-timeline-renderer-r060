package audit

import (
	"context"
	"fmt"
	"math"
)

// rebuildError marks a reconstruction that cannot be attempted because the
// declared evidence is incomplete or internally inconsistent. It maps to an
// INSUFFICIENT_EVIDENCE outcome, never to a silent pass.
type rebuildError struct {
	Reason string
}

func (e *rebuildError) Error() string { return e.Reason }

func rebuildFailure(format string, args ...any) error {
	return &rebuildError{Reason: fmt.Sprintf(format, args...)}
}

// RebuildInput carries everything an independent reviewer has: exactly what
// was selected from each source, the processing conditions that were adopted,
// the downloaded PCM, and the receipt.
type RebuildInput struct {
	Candidate        *Candidate
	Sources          map[string]*PCM
	Downloaded       *PCM
	DownloadedDigest string
	Receipt          *ReceiptEvidence
	Targets          FidelityTargets
	RuleSetID        string
	BuiltAt          string
}

// Rebuild reconstructs the candidate's PCM solely from the declared source
// selections and processing conditions, then checks it against the downloaded
// PCM and the receipt. A mismatch within the declared quantization tolerance
// still counts as a reconstruction; a mismatch beyond it means the record
// does not explain the delivered bytes.
//
// The reconstruction never reads the production renderer. It is an
// independent reference implementation over integer source bytes, so a
// reviewer can reproduce it with any conforming toolchain.
func Rebuild(ctx context.Context, input RebuildInput) (Reconstruction, []Finding, error) {
	downloaded := input.Downloaded
	reconstruction := Reconstruction{
		CandidateID:         input.CandidateID(),
		RuleSetID:           input.RuleSetID,
		BuiltAt:             input.BuiltAt,
		DownloadedPCMDigest: input.DownloadedDigest,
	}
	if input.Candidate == nil || downloaded == nil {
		return reconstruction, nil, rebuildFailure("candidate or downloaded PCM is missing")
	}
	candidate := input.Candidate
	reconstruction.DownloadedFrames = downloaded.FrameCount
	if len(candidate.Spans) == 0 {
		return reconstruction, nil, rebuildFailure("candidate declares no spans")
	}

	findings := make([]Finding, 0)
	// The record's declaration and source bytes are enough to produce a
	// reference rendering; whether it reproduces the download is decided
	// below and reported as a finding rather than by clearing this flag.
	reconstruction.Available = true

	// --- Structural sanity of the declared format --------------------------
	if candidate.SampleRate != downloaded.SampleRate ||
		candidate.OutputChannels != downloaded.Channels ||
		downloaded.BitsPerSample != 16 {
		findings = append(findings, Finding{
			Class:      "declared_format_does_not_match_download",
			Attributes: []string{"processing", "download"},
			Detail: fmt.Sprintf(
				"candidate declares %d Hz/%d ch but the download is %d Hz/%d ch/16-bit",
				candidate.SampleRate, candidate.OutputChannels,
				downloaded.SampleRate, downloaded.Channels,
			),
			Blocking: true,
		})
		return reconstruction, findings, nil
	}

	// --- Reference rebuild --------------------------------------------------
	referenceBytes, referencePCM, err := referenceRender(ctx, candidate, input.Sources)
	if err != nil {
		return reconstruction, nil, err
	}
	reconstruction.ExpectedFrames = referencePCM.FrameCount
	reconstruction.ReferencePCMDigest = digestBytes(referenceBytes)
	// Declared resampling uses one rounding convention; an independent
	// rebuild may land a single frame away at a rate boundary. One frame is a
	// reconstruction slack, not a release allowance.
	reconstruction.FramesMatch = absInt64(referencePCM.FrameCount-downloaded.FrameCount) <= 1

	// --- Reconstruction versus downloaded PCM ------------------------------
	if !reconstruction.FramesMatch {
		findings = append(findings, Finding{
			Class:      "reconstruction_frame_count_mismatch",
			Attributes: []string{"selection", "processing", "download"},
			Detail: fmt.Sprintf(
				"declared selection and processing predict %d frames but the download has %d",
				referencePCM.FrameCount, downloaded.FrameCount,
			),
			Magnitude: float64(referencePCM.FrameCount - downloaded.FrameCount),
			Blocking:  true,
		})
	} else {
		comparison, compareErr := CompareWaveforms(ctx, referencePCM, downloaded, input.Targets.QuantizationToleranceLSB)
		if compareErr != nil {
			return reconstruction, nil, compareErr
		}
		reconstruction.MaxResidualLSB = comparison.MaxResidualLSB
		reconstruction.MeanResidualLSB = comparison.MeanResidualLSB
		reconstruction.ResidualAvailable = true
		// The reference rebuild legitimately adds one extra integer
		// round-trip over the producer's own output. The explainability
		// threshold is therefore wider than the release quantization
		// tolerance; it exists to catch a record that does not describe the
		// download at all, not to re-litigate one-LSB rounding.
		explainTolerance := explainabilityTolerance(input.Targets.QuantizationToleranceLSB)
		reconstruction.MatchesDownloaded = comparison.Comparable &&
			comparison.MaxResidualLSB <= explainTolerance &&
			math.Abs(comparison.FullLagSamples) <= PhaseToleranceSamples &&
			comparison.LagDriftSamples <= LagDriftToleranceSamples &&
			!comparison.PolarityInverted
		if !reconstruction.MatchesDownloaded {
			findings = append(findings, Finding{
				Class:      "declared_processing_does_not_explain_download",
				Attributes: []string{"selection", "processing", "download"},
				Detail: fmt.Sprintf(
					"reference rebuilt from the declared selection and conditions differs from the download (max residual %.2f LSB, mean %.2f LSB, lag %.2f samples, lag drift %.2f samples, polarity inverted=%t); explainability tolerance %.2f LSB",
					reconstruction.MaxResidualLSB, reconstruction.MeanResidualLSB,
					comparison.FullLagSamples, comparison.LagDriftSamples, comparison.PolarityInverted, explainTolerance,
				),
				Magnitude: reconstruction.MaxResidualLSB,
				Blocking:  true,
			})
		} else {
			reconstruction.Notes = append(reconstruction.Notes,
				fmt.Sprintf("reference rebuild reproduces the download within %.2f LSB", explainTolerance))
		}
	}

	// --- Receipt versus downloaded PCM and declared choices ---------------
	receiptAudit, receiptFindings := auditReceipt(input, candidate)
	reconstruction.Receipt = receiptAudit
	findings = append(findings, receiptFindings...)

	// --- Selection sanity ---------------------------------------------------
	findings = append(findings, auditSelections(candidate, input.Sources)...)

	if len(findings) > 0 {
		return reconstruction, findings, nil
	}
	return reconstruction, nil, nil
}

func (input RebuildInput) CandidateID() string {
	if input.Candidate == nil {
		return ""
	}
	return input.Candidate.ID
}

// referenceRender rebuilds the output bytes from the declared selection and
// processing conditions. It returns the encoded WAV and its decoded view.
func referenceRender(ctx context.Context, candidate *Candidate, sources map[string]*PCM) ([]byte, *PCM, error) {
	totalFrames, clips, err := rebuildSpans(ctx, candidate, sources)
	if err != nil {
		return nil, nil, err
	}
	mix, err := mixRebuild(ctx, clips, totalFrames, candidate.OutputChannels)
	if err != nil {
		return nil, nil, err
	}
	mix = applyPostProcessing(mix, candidate.Post)
	if ratio := candidate.Post.OutputClockRatio; ratio != 0 && ratio != 1 {
		stretched, stretchErr := stretchTimebase(ctx, mix, candidate.OutputChannels, ratio)
		if stretchErr != nil {
			return nil, nil, stretchErr
		}
		mix = stretched
	}
	quantized := Quantize16(mix, candidate.Post.Quantization)
	encoded, err := EncodePCM16Wave(candidate.SampleRate, candidate.OutputChannels, quantized)
	if err != nil {
		return nil, nil, err
	}
	decoded, err := DecodePCM16Wave(encoded)
	if err != nil {
		return nil, nil, err
	}
	return encoded, decoded, nil
}

// ReferenceRender is the exported review-side rebuild, used by the
// demonstration to state what the record predicts without calling the
// production renderer.
func ReferenceRender(ctx context.Context, candidate *Candidate, sources map[string]*PCM) ([]byte, error) {
	encoded, _, err := referenceRender(ctx, candidate, sources)
	return encoded, err
}

type rebuiltClip struct {
	Index            int
	SourceStartFrame int64
	SourceEndFrame   int64
	OutputStartFrame int64
	OutputFrames     int64
	CrossfadeFrames  int64
	Samples          []float64
}

func rebuildSpans(ctx context.Context, candidate *Candidate, sources map[string]*PCM) (int64, []rebuiltClip, error) {
	cursor := int64(0)
	clips := make([]rebuiltClip, 0, len(candidate.Spans))
	for index, span := range candidate.Spans {
		if err := readContextErr(ctx); err != nil {
			return 0, nil, err
		}
		source, ok := sources[span.Selection.SourceID]
		if !ok || source == nil {
			return 0, nil, rebuildFailure("source %q required by span %d is not in the record", span.Selection.SourceID, index)
		}
		start := span.Selection.SourceStartFrame
		end := span.Selection.SourceEndFrame
		if start < 0 || end <= start || end > source.FrameCount {
			return 0, nil, rebuildFailure(
				"span %d selects frames [%d,%d) outside source %q with %d frames",
				index, start, end, span.Selection.SourceID, source.FrameCount,
			)
		}
		if span.Selection.SourceSampleRate != 0 && span.Selection.SourceSampleRate != source.SampleRate {
			return 0, nil, rebuildFailure(
				"span %d declares source rate %d but stored source %q is %d Hz",
				index, span.Selection.SourceSampleRate, span.Selection.SourceID, source.SampleRate,
			)
		}
		if span.Selection.SourceChannels != 0 && span.Selection.SourceChannels != source.Channels {
			return 0, nil, rebuildFailure(
				"span %d declares %d source channels but stored source %q has %d",
				index, span.Selection.SourceChannels, span.Selection.SourceID, source.Channels,
			)
		}

		mapped, err := rebuildSpan(ctx, source, span, candidate.SampleRate, candidate.OutputChannels)
		if err != nil {
			return 0, nil, err
		}
		frames := int64(len(mapped) / candidate.OutputChannels)
		if frames < 1 {
			return 0, nil, rebuildFailure("span %d produces no output frames", index)
		}
		outputStart := cursor
		crossfade := span.Processing.CrossfadeFrames
		if index > 0 {
			outputStart -= crossfade
		}
		if outputStart < 0 {
			return 0, nil, rebuildFailure("span %d has a negative output position", index)
		}
		if index > 0 && crossfade > 0 {
			previousFrames := clips[len(clips)-1].OutputFrames
			if crossfade > previousFrames || crossfade > frames {
				return 0, nil, rebuildFailure(
					"span %d crossfade of %d frames exceeds a neighboring span (%d, %d)",
					index, crossfade, previousFrames, frames,
				)
			}
		}
		cursor = outputStart + frames
		clips = append(clips, rebuiltClip{
			Index:            index,
			SourceStartFrame: start,
			SourceEndFrame:   end,
			OutputStartFrame: outputStart,
			OutputFrames:     frames,
			CrossfadeFrames:  crossfade,
			Samples:          mapped,
		})
	}
	return cursor, clips, nil
}

// rebuildSpan performs slice -> resample -> channel map -> gain, exactly as
// declared, without consulting the production renderer.
func rebuildSpan(ctx context.Context, source *PCM, span CandidateSpan, targetRate, targetChannels int) ([]float64, error) {
	// Slice in the source's own layout.
	start := int(span.Selection.SourceStartFrame)
	end := int(span.Selection.SourceEndFrame)
	channels := source.Channels
	sliced := make([]float64, (end-start)*channels)
	for index := range sliced {
		sliced[index] = normalizedSample(source.Samples[start*channels+index])
	}

	method := span.Processing.ResampleMethod
	if method == "" {
		method = ResampleLinear
	}
	resampled, err := resampleDeclared(ctx, sliced, source.SampleRate, targetRate, channels, method)
	if err != nil {
		return nil, err
	}

	mapping := span.Processing.ChannelMapping
	if mapping == "" {
		mapping = MappingUnchanged
	}
	mapped, err := mapChannelsDeclared(resampled, channels, targetChannels, mapping)
	if err != nil {
		return nil, err
	}

	gain := math.Pow(10, span.Processing.GainDB/20)
	for index := range mapped {
		mapped[index] *= gain
	}
	return mapped, nil
}

// resampleDeclared implements exactly the two declared converters. Any other
// name is insufficient evidence, not a guess.
func resampleDeclared(ctx context.Context, interleaved []float64, sourceRate, targetRate, channels int, method string) ([]float64, error) {
	if sourceRate < 8000 || sourceRate > 192000 || targetRate < 8000 || targetRate > 192000 {
		return nil, rebuildFailure("sample rate outside the supported range")
	}
	sourceFrames := int64(len(interleaved) / channels)
	targetFrames := ScaledFrames(sourceFrames, sourceRate, targetRate)
	result := make([]float64, int(targetFrames)*channels)
	switch method {
	case ResampleLinear:
		step := float64(sourceRate) / float64(targetRate)
		for frame := int64(0); frame < targetFrames; frame++ {
			if frame%4096 == 0 {
				if err := readContextErr(ctx); err != nil {
					return nil, err
				}
			}
			position := float64(frame) * step
			left := int64(position)
			if left >= sourceFrames-1 {
				left = sourceFrames - 1
			}
			if left < 0 {
				left = 0
			}
			right := left + 1
			if right >= sourceFrames {
				right = sourceFrames - 1
			}
			weight := position - float64(left)
			for channel := 0; channel < channels; channel++ {
				first := interleaved[left*int64(channels)+int64(channel)]
				second := interleaved[right*int64(channels)+int64(channel)]
				result[frame*int64(channels)+int64(channel)] = first + (second-first)*weight
			}
		}
	case ResampleNearest:
		step := float64(sourceRate) / float64(targetRate)
		for frame := int64(0); frame < targetFrames; frame++ {
			position := int64(float64(frame) * step)
			if position >= sourceFrames {
				position = sourceFrames - 1
			}
			for channel := 0; channel < channels; channel++ {
				result[frame*int64(channels)+int64(channel)] = interleaved[position*int64(channels)+int64(channel)]
			}
		}
	default:
		return nil, rebuildFailure("resample method %q is not in the declared rule table", method)
	}
	return result, nil
}

func mapChannelsDeclared(interleaved []float64, sourceChannels, targetChannels int, method string) ([]float64, error) {
	if sourceChannels == targetChannels {
		if method != MappingUnchanged && method != "" {
			return nil, rebuildFailure("channel mapping %q contradicts the declared channel counts", method)
		}
		result := make([]float64, len(interleaved))
		copy(result, interleaved)
		return result, nil
	}
	frames := int64(len(interleaved) / sourceChannels)
	result := make([]float64, int(frames)*targetChannels)
	switch {
	case sourceChannels == 1 && targetChannels == 2 && method == MappingMonoToStereo:
		for frame := int64(0); frame < frames; frame++ {
			value := interleaved[frame]
			result[frame*2] = value
			result[frame*2+1] = value
		}
	case sourceChannels == 2 && targetChannels == 1 && method == MappingStereoToMono:
		for frame := int64(0); frame < frames; frame++ {
			result[frame] = (interleaved[frame*2] + interleaved[frame*2+1]) * 0.5
		}
	default:
		return nil, rebuildFailure("channel mapping %q is not valid for %d->%d channels", method, sourceChannels, targetChannels)
	}
	return result, nil
}

func mixRebuild(ctx context.Context, clips []rebuiltClip, totalFrames int64, channels int) ([]float64, error) {
	if totalFrames < 1 || channels < 1 {
		return nil, rebuildFailure("mix dimensions are invalid")
	}
	mix := make([]float64, int(totalFrames)*channels)
	for _, clip := range clips {
		frames := clip.OutputFrames
		for frame := int64(0); frame < frames; frame++ {
			if frame%4096 == 0 {
				if err := readContextErr(ctx); err != nil {
					return nil, err
				}
			}
			incoming := 1.0
			outgoing := 1.0
			if clip.CrossfadeFrames > 0 && frame < clip.CrossfadeFrames {
				progress := (float64(frame) + 0.5) / float64(clip.CrossfadeFrames)
				angle := progress * math.Pi / 2
				incoming = math.Sin(angle)
				outgoing = math.Cos(angle)
			}
			for channel := 0; channel < channels; channel++ {
				inStart := int(frame)*channels + channel
				outStart := int(clip.OutputStartFrame+frame)*channels + channel
				value := clip.Samples[inStart] * incoming
				if clip.CrossfadeFrames > 0 && frame < clip.CrossfadeFrames {
					mix[outStart] *= outgoing
				}
				mix[outStart] += value
			}
		}
	}
	return mix, nil
}

func applyPostProcessing(mix []float64, post PostProcessing) []float64 {
	if post.PolarityInverted {
		for index := range mix {
			mix[index] = -mix[index]
		}
	}
	if post.AppliedGainDB != 0 {
		gain := math.Pow(10, post.AppliedGainDB/20)
		for index := range mix {
			mix[index] *= gain
		}
	}
	if post.PhaseDelayFrames != 0 {
		// The declared field is a delay: positive values move the signal later
		// in time. shiftAndInterpolate treats a positive lag as advancing the
		// signal, so the sign is inverted here deliberately.
		shifted := shiftAndInterpolate(mix, -post.PhaseDelayFrames)
		copy(mix, shifted)
	}
	return mix
}

// stretchTimebase linearly resamples the whole mix by a declared clock ratio.
// It exists so a candidate with a slightly wrong output clock can declare that
// condition and be reconstructed, rather than having its drift silently
// attributed to resampling quality.
func stretchTimebase(ctx context.Context, mix []float64, channels int, ratio float64) ([]float64, error) {
	if !finite(ratio) || ratio < 0.5 || ratio > 2 {
		return nil, rebuildFailure("output clock ratio %.6f is outside the declarable range", ratio)
	}
	sourceFrames := int64(len(mix) / channels)
	targetFrames := int64(math.Round(float64(sourceFrames) * ratio))
	if targetFrames < 1 {
		return nil, rebuildFailure("output clock ratio produces no frames")
	}
	result := make([]float64, int(targetFrames)*channels)
	for frame := int64(0); frame < targetFrames; frame++ {
		if frame%4096 == 0 {
			if err := readContextErr(ctx); err != nil {
				return nil, err
			}
		}
		position := float64(frame) / ratio
		left := int64(position)
		if left >= sourceFrames-1 {
			left = sourceFrames - 1
		}
		right := left + 1
		if right >= sourceFrames {
			right = sourceFrames - 1
		}
		weight := position - float64(left)
		for channel := 0; channel < channels; channel++ {
			first := mix[left*int64(channels)+int64(channel)]
			second := mix[right*int64(channels)+int64(channel)]
			result[frame*int64(channels)+int64(channel)] = first + (second-first)*weight
		}
	}
	return result, nil
}

// auditSelections verifies that the declared selection facts agree with the
// stored source bytes that the record holds.
func auditSelections(candidate *Candidate, sources map[string]*PCM) []Finding {
	var findings []Finding
	for index, span := range candidate.Spans {
		source, ok := sources[span.Selection.SourceID]
		if !ok || source == nil {
			findings = append(findings, Finding{
				Class:      "selection_source_missing",
				Attributes: []string{fmt.Sprintf("span:%d", index), "selection"},
				Detail:     fmt.Sprintf("span %d references source %q but its bytes are not in the record", index, span.Selection.SourceID),
				Blocking:   true,
			})
			continue
		}
		if span.Selection.SourceSampleRate != 0 && span.Selection.SourceSampleRate != source.SampleRate {
			findings = append(findings, Finding{
				Class:      "selection_source_rate_mismatch",
				Attributes: []string{fmt.Sprintf("span:%d", index), "selection"},
				Detail:     fmt.Sprintf("span %d declares source rate %d but the stored source is %d Hz", index, span.Selection.SourceSampleRate, source.SampleRate),
				Magnitude:  float64(span.Selection.SourceSampleRate - source.SampleRate),
				Blocking:   true,
			})
		}
		if span.Selection.SourceChannels != 0 && span.Selection.SourceChannels != source.Channels {
			findings = append(findings, Finding{
				Class:      "selection_source_channels_mismatch",
				Attributes: []string{fmt.Sprintf("span:%d", index), "selection"},
				Detail:     fmt.Sprintf("span %d declares %d source channels but the stored source has %d", index, span.Selection.SourceChannels, source.Channels),
				Magnitude:  float64(span.Selection.SourceChannels - source.Channels),
				Blocking:   true,
			})
		}
	}
	return findings
}

func auditReceipt(input RebuildInput, candidate *Candidate) (ReceiptAudit, []Finding) {
	receiptAudit := ReceiptAudit{}
	receipt := input.Receipt
	if receipt == nil || !receipt.Present {
		receiptAudit.Detail = "no receipt evidence was recorded"
		return receiptAudit, []Finding{{
			Class:      "receipt_missing",
			Attributes: []string{"receipt", "insufficient"},
			Detail:     "no receipt was recorded, so the delivered bytes cannot be reconciled with a declared result",
			Blocking:   true,
		}}
	}
	receiptAudit.Present = true
	var findings []Finding

	receiptAudit.DigestMatchesPCM = receipt.OutputDigest == input.DownloadedDigest
	if !receiptAudit.DigestMatchesPCM {
		findings = append(findings, Finding{
			Class:      "receipt_digest_mismatch",
			Attributes: []string{"receipt", "download"},
			Detail:     "receipt output digest does not match the downloaded PCM bytes",
			Blocking:   true,
		})
	}
	receiptAudit.FrameCountMatches = receipt.OutputFrameCount == input.Downloaded.FrameCount
	if !receiptAudit.FrameCountMatches {
		findings = append(findings, Finding{
			Class:      "receipt_frame_count_mismatch",
			Attributes: []string{"receipt", "download"},
			Detail:     fmt.Sprintf("receipt declares %d frames but the download has %d", receipt.OutputFrameCount, input.Downloaded.FrameCount),
			Magnitude:  float64(receipt.OutputFrameCount - input.Downloaded.FrameCount),
			Blocking:   true,
		})
	}

	observation, err := ObservePCM(context.Background(), candidate.ID, input.DownloadedDigest, input.Downloaded, "")
	if err == nil {
		receiptAudit.PeakMatches = math.Abs(receipt.OutputPeakDBFS-observation.PeakDBFS) <= input.Targets.PeakToleranceDB
		receiptAudit.RMSMatches = math.Abs(receipt.OutputRMSDBFS-observation.RMSDBFS) <= input.Targets.LoudnessToleranceDB
		if !receiptAudit.PeakMatches {
			findings = append(findings, Finding{
				Class:      "receipt_peak_disagrees_with_download",
				Attributes: []string{"receipt", "download"},
				Detail:     fmt.Sprintf("receipt reports peak %.3f dBFS but the download measures %.3f dBFS", receipt.OutputPeakDBFS, observation.PeakDBFS),
				Magnitude:  math.Abs(receipt.OutputPeakDBFS - observation.PeakDBFS),
				Blocking:   true,
			})
		}
		if !receiptAudit.RMSMatches {
			findings = append(findings, Finding{
				Class:      "receipt_rms_disagrees_with_download",
				Attributes: []string{"receipt", "download"},
				Detail:     fmt.Sprintf("receipt reports RMS %.3f dBFS but the download measures %.3f dBFS", receipt.OutputRMSDBFS, observation.RMSDBFS),
				Magnitude:  math.Abs(receipt.OutputRMSDBFS - observation.RMSDBFS),
				Blocking:   true,
			})
		}
	}

	receiptAudit.SpansMatchSelection = receiptSpansMatchDeclared(receipt, candidate)
	if !receiptAudit.SpansMatchSelection {
		findings = append(findings, Finding{
			Class:      "receipt_spans_disagree_with_selection",
			Attributes: []string{"receipt", "selection"},
			Detail:     "receipt span placement, source identifiers, or crossfade do not match the declared selection",
			Blocking:   true,
		})
	}

	receiptAudit.NormalizationMatches = math.Abs(receipt.RequestedPeakDBFS-candidate.Post.NormalizationTargetDBFS) <= 1e-6
	if !receiptAudit.NormalizationMatches {
		findings = append(findings, Finding{
			Class:      "receipt_normalization_target_mismatch",
			Attributes: []string{"receipt", "processing"},
			Detail:     fmt.Sprintf("receipt requested peak %.3f dBFS but the candidate declares %.3f dBFS", receipt.RequestedPeakDBFS, candidate.Post.NormalizationTargetDBFS),
			Magnitude:  math.Abs(receipt.RequestedPeakDBFS - candidate.Post.NormalizationTargetDBFS),
			Blocking:   true,
		})
	}
	return receiptAudit, findings
}

func receiptSpansMatchDeclared(receipt *ReceiptEvidence, candidate *Candidate) bool {
	if len(receipt.Spans) != len(candidate.Spans) {
		return false
	}
	for index := range candidate.Spans {
		declared := candidate.Spans[index]
		actual := receipt.Spans[index]
		if actual.SourceID != declared.Selection.SourceID ||
			actual.SourceStartFrame != declared.Selection.SourceStartFrame ||
			actual.SourceEndFrame != declared.Selection.SourceEndFrame ||
			actual.Index != declared.Index ||
			actual.CrossfadeFrames != declared.Processing.CrossfadeFrames {
			return false
		}
		if declared.Selection.SourceDigest != "" && actual.SourceDigest != declared.Selection.SourceDigest {
			return false
		}
	}
	return true
}

// ScaledFrames mirrors the domain's rounding convention for resampled frame
// counts. It is repeated here deliberately so the audit does not depend on
// production code.
func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func explainabilityTolerance(quantizationToleranceLSB float64) float64 {
	// The review-side rebuild re-derives the producer's output from source
	// bytes, so it pays one extra integer round-trip before comparison. The
	// extra LSB is a property of reconstruction, not a release allowance.
	extra := quantizationToleranceLSB + 1.5
	if extra < 2 {
		extra = 2
	}
	return extra
}

// ScaledFrames mirrors the domain's rounding convention for resampled frame
// counts. It is repeated here deliberately so the audit does not depend on
// production code.
func ScaledFrames(frameCount int64, sourceRate, targetRate int) int64 {
	if frameCount <= 0 || sourceRate <= 0 || targetRate <= 0 {
		return 0
	}
	return int64(math.Round(float64(frameCount) * float64(targetRate) / float64(sourceRate)))
}
