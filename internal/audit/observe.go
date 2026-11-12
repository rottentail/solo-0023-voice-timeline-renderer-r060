package audit

import (
	"context"
	"math"
	"strings"
)

// monoFrames collapses an interleaved payload to one review-domain sequence.
func monoFrames(samples []int16, channels int) []float64 {
	frames := len(samples) / channels
	result := make([]float64, frames)
	if channels == 1 {
		for frame := 0; frame < frames; frame++ {
			result[frame] = normalizedSample(samples[frame])
		}
		return result
	}
	for frame := 0; frame < frames; frame++ {
		result[frame] = (normalizedSample(samples[frame*2]) + normalizedSample(samples[frame*2+1])) * 0.5
	}
	return result
}

// ObservePCM measures a decoded PCM payload. All measurements are derived
// from integer samples so that an independent reviewer can reproduce them
// without the production codec.
func ObservePCM(ctx context.Context, candidateID, pcmDigest string, pcm *PCM, measuredAt string) (Observation, error) {
	observation := Observation{
		CandidateID:   candidateID,
		MeasuredAt:    measuredAt,
		PCMDigest:     pcmDigest,
		PCMBytes:      pcm.ByteCount,
		FrameCount:    pcm.FrameCount,
		SampleRate:    pcm.SampleRate,
		Channels:      pcm.Channels,
		BitsPerSample: pcm.BitsPerSample,
		DurationMS:    durationMilliseconds(pcm.FrameCount, pcm.SampleRate),
		PeakDBFS:      silenceFloorDBFS,
		RMSDBFS:       silenceFloorDBFS,
	}
	if pcm.FrameCount < 1 || pcm.Channels < 1 {
		return observation, nil
	}

	peakAbs := 0.0
	peakFrame := int64(0)
	sumSquares := 0.0
	sumAbs := 0.0
	var clipped int64
	var active int64
	frames := pcm.FrameCount
	channels := pcm.Channels
	for frame := int64(0); frame < frames; frame++ {
		if frame%8192 == 0 {
			if err := readContextErr(ctx); err != nil {
				return Observation{}, err
			}
		}
		framePeak := 0.0
		base := int(frame) * channels
		for channel := 0; channel < channels; channel++ {
			raw := pcm.Samples[base+channel]
			if raw == 32767 || raw == -32768 {
				clipped++
			}
			value := normalizedSample(raw)
			absolute := math.Abs(value)
			if absolute > framePeak {
				framePeak = absolute
			}
			if absolute >= ActiveSampleThreshold {
				active++
			}
			sumSquares += value * value
			sumAbs += absolute
		}
		if framePeak > peakAbs {
			peakAbs = framePeak
			peakFrame = frame
		}
	}
	totalSamples := float64(frames) * float64(channels)
	observation.PeakSampleAbs = peakAbs
	observation.PeakSampleFrame = peakFrame
	observation.PeakDBFS = amplitudeDBFS(peakAbs)
	observation.RMSDBFS = amplitudeDBFS(math.Sqrt(sumSquares / totalSamples))
	observation.MeanAbsSample = sumAbs / totalSamples
	observation.ClippedSampleCount = clipped
	observation.ActiveFrameCount = active
	return observation, nil
}

// CompareStructure checks format and timing without touching waveform
// samples, so converter errors are attributable before deeper comparison.
func CompareStructure(baseline, candidate *PCM) StructuralComparison {
	comparison := StructuralComparison{
		RateMatches:         baseline.SampleRate == candidate.SampleRate,
		ChannelsMatch:       baseline.Channels == candidate.Channels,
		BitsMatch:           baseline.BitsPerSample == candidate.BitsPerSample,
		FramesMatch:         baseline.FrameCount == candidate.FrameCount,
		BaselineSampleRate:  baseline.SampleRate,
		CandidateSampleRate: candidate.SampleRate,
		BaselineChannels:    baseline.Channels,
		CandidateChannels:   candidate.Channels,
		BaselineBits:        baseline.BitsPerSample,
		CandidateBits:       candidate.BitsPerSample,
		BaselineFrames:      baseline.FrameCount,
		CandidateFrames:     candidate.FrameCount,
	}
	if baseline.SampleRate > 0 {
		comparison.DurationDeltaMS = math.Abs(
			float64(candidate.FrameCount-baseline.FrameCount) * 1000.0 / float64(baseline.SampleRate),
		)
	}
	return comparison
}

// CompareWaveforms aligns two same-format renderings and separates the
// adjacent causes of a difference:
//
//   - a constant lag is a phase shift,
//   - lag drift across the duration is a resampling/timebase change,
//   - residual after alignment is a waveform change; a residual inside the
//     declared quantization tolerance is only requantization noise, and a
//     residual explained by one fitted scalar gain is only level.
func CompareWaveforms(ctx context.Context, baseline, candidate *PCM, toleranceLSB float64) (WaveformComparison, error) {
	comparison := WaveformComparison{}
	if baseline.SampleRate != candidate.SampleRate ||
		baseline.Channels != candidate.Channels ||
		baseline.BitsPerSample != candidate.BitsPerSample {
		comparison.Reason = "output formats differ; waveform alignment is not meaningful"
		return comparison, nil
	}
	if baseline.FrameCount < 1 || candidate.FrameCount < 1 {
		comparison.Reason = "empty payload"
		return comparison, nil
	}
	// Different lengths are still comparable: the drift measurement is exactly
	// what distinguishes a timebase change from a constant phase shift. The
	// common prefix is used, and the residual check is only applied when the
	// lengths match.
	comparison.FrameCountsDiffer = baseline.FrameCount != candidate.FrameCount

	baselineFrames := monoFrames(baseline.Samples, baseline.Channels)
	candidateFrames := monoFrames(candidate.Samples, candidate.Channels)
	commonLength := len(baselineFrames)
	if len(candidateFrames) < commonLength {
		commonLength = len(candidateFrames)
	}
	if commonLength > maxAnalysisFrames {
		commonLength = maxAnalysisFrames
	}
	baselineFrames = baselineFrames[:commonLength]
	candidateFrames = candidateFrames[:commonLength]

	signed := measureLagSigned(baselineFrames, candidateFrames, minSearchLag, commonLength)
	magnitude := measureLagMagnitude(baselineFrames, candidateFrames, minSearchLag, commonLength)
	alignFrames := candidateFrames
	if magnitude.Valid && magnitude.Correlation < -0.5 &&
		(!signed.Valid || math.Abs(magnitude.Correlation) >= math.Abs(signed.Correlation)) {
		// The delivery is globally inverted: the magnitude search recovers
		// the true delay while the signed search latches onto a spurious
		// positive lobe. Canonicalize polarity before measuring drift and
		// residual, so a phase-class change is not mistaken for waveform
		// noise.
		comparison.PolarityInverted = true
		alignFrames = negate(candidateFrames)
	}
	estimate := measureLagSigned(baselineFrames, alignFrames, minSearchLag, commonLength)
	if !estimate.Valid {
		comparison.Reason = "signal is too short or too quiet to align"
		return comparison, nil
	}
	comparison.Comparable = true
	comparison.FullLagSamples = estimate.Lag
	comparison.FullCorrelation = estimate.Correlation

	if scans, ok := scanSegments(baselineFrames, alignFrames, minSearchLag); ok {
		comparison.SegmentLagsAvailable = true
		comparison.StartLagSamples = scans[0].Lag
		comparison.EndLagSamples = scans[len(scans)-1].Lag
		comparison.MidLagSamples = medianLag(scans)
		comparison.LagDriftSamples = segmentLagDrift(scans)
	}

	chosen := alignedResidual(baselineFrames, alignFrames, estimate.Lag, toleranceLSB)
	comparison.FittedGainDB = amplitudeDBFS(math.Abs(chosen.FitGain))
	comparison.MaxResidualLSB = chosen.Max
	comparison.MeanResidualLSB = chosen.Mean
	comparison.QuantizationTolerance = toleranceLSB
	comparison.ResidualComparable = !comparison.FrameCountsDiffer
	if comparison.ResidualComparable {
		comparison.ResidualOverToleranceCount = chosen.Over
		comparison.ResidualSampleCount = chosen.Count
	}

	baselineObservation, err := ObservePCM(ctx, "", "", baseline, "")
	if err != nil {
		return WaveformComparison{}, err
	}
	candidateObservation, err := ObservePCM(ctx, "", "", candidate, "")
	if err != nil {
		return WaveformComparison{}, err
	}
	comparison.RMSDeltaDB = candidateObservation.RMSDBFS - baselineObservation.RMSDBFS
	comparison.PeakDeltaDB = candidateObservation.PeakDBFS - baselineObservation.PeakDBFS
	return comparison, nil
}

func negate(samples []float64) []float64 {
	result := make([]float64, len(samples))
	for index, value := range samples {
		result[index] = -value
	}
	return result
}

func amplitudeDBFS(value float64) float64 {
	if value <= 0 || math.IsNaN(value) {
		return silenceFloorDBFS
	}
	measured := 20 * math.Log10(value)
	if measured < silenceFloorDBFS {
		return silenceFloorDBFS
	}
	return measured
}

func durationMilliseconds(frames int64, rate int) float64 {
	if frames <= 0 || rate <= 0 {
		return 0
	}
	return float64(frames) * 1000.0 / float64(rate)
}

// applyDeclaredDefaults fills in the review convention when a producer
// omitted a processing field, so that a reconstruction is always possible.
func applyDeclaredDefaults(candidate *Candidate) {
	if candidate == nil {
		return
	}
	if strings.TrimSpace(candidate.Post.Quantization) == "" {
		candidate.Post.Quantization = QuantRoundHalfAway
	}
	for index := range candidate.Spans {
		span := &candidate.Spans[index]
		if strings.TrimSpace(span.Processing.ResampleMethod) == "" {
			span.Processing.ResampleMethod = ResampleLinear
		}
		if strings.TrimSpace(span.Processing.ChannelMapping) == "" {
			span.Processing.ChannelMapping = MappingUnchanged
		}
	}
}
