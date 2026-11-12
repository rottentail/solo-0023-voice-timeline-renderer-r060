package audit

import (
	"fmt"
	"math"
	"strings"
)

// RenderReport writes the human-readable review document for a verdict. The
// document is derived only from stored facts, so a reviewer can restate every
// decision by pointing at the record.
func RenderReport(record ChangeRecord, verdict Verdict) string {
	var builder strings.Builder
	builder.WriteString("# Audio change review\n\n")
	builder.WriteString(fmt.Sprintf("- Record: `%s` (%s)\n", record.RecordID, record.Title))
	builder.WriteString(fmt.Sprintf("- Candidate: `%s`\n", verdict.CandidateID))
	builder.WriteString(fmt.Sprintf("- Baseline candidate: `%s`\n", verdict.BaselineCandidateID))
	builder.WriteString(fmt.Sprintf("- Rule set: `%s`\n", verdict.RuleSetID))
	builder.WriteString(fmt.Sprintf("- Decided at: `%s`\n", verdict.DecidedAt))
	builder.WriteString(fmt.Sprintf("- Outcome: **%s**\n\n", plainOutcome(verdict.Outcome)))

	builder.WriteString("## Historical byte promise versus signal-quality target\n\n")
	builder.WriteString(fmt.Sprintf(
		"The record was created with these captured targets. They were not recomputed from any later render:\n\n"+
			"- Format: %d Hz, %d channel(s), %d-bit PCM\n"+
			"- Target peak: %.2f dBFS (tolerance +-%.2f dB)\n"+
			"- Loudness continuity: +-%.2f dB against the baseline\n"+
			"- Quantization tolerance: %.2f LSB\n"+
			"- Duration tolerance: %.2f ms\n",
		record.FidelityTargets.SampleRate, record.FidelityTargets.Channels, record.FidelityTargets.BitsPerSample,
		record.FidelityTargets.TargetPeakDBFS, record.FidelityTargets.PeakToleranceDB,
		record.FidelityTargets.LoudnessToleranceDB,
		record.FidelityTargets.QuantizationToleranceLSB,
		record.FidelityTargets.DurationToleranceMS,
	))
	if record.FidelityTargets.HistoricalOutputDigest != "" {
		builder.WriteString(fmt.Sprintf("- Historical delivery digest: `%s` (%d bytes)\n",
			record.FidelityTargets.HistoricalOutputDigest, record.FidelityTargets.HistoricalOutputBytes))
	} else {
		builder.WriteString("- Historical delivery digest: not declared\n")
	}
	builder.WriteString(fmt.Sprintf("\nDownloaded PCM digest: `%s`\n\n", verdict.DownloadedDigest))
	builder.WriteString("Byte identity to the baseline is the strongest outcome. When the bytes differ, " +
		"the rules accept the change only if every difference falls in a tolerated class. " +
		"Small is not automatically harmless: a small unbounded waveform change blocks release, " +
		"while a legal numeric difference such as requantization noise is accepted.\n\n")

	builder.WriteString("## Checks\n\n")
	builder.WriteString("| Rule | Result | Measured | Limit | Unit |\n")
	builder.WriteString("|---|---|---|---|---|\n")
	for _, check := range verdict.Checks {
		status := "pass"
		if !check.Passed {
			status = "FAIL"
		}
		builder.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
			check.Name, status, formatValue(check.Value, check.Unit), formatValue(check.Limit, check.Unit), check.Unit))
	}
	builder.WriteString("\n")

	if len(verdict.Reasons) > 0 {
		builder.WriteString("## Reasons\n\n")
		for _, reason := range verdict.Reasons {
			builder.WriteString(fmt.Sprintf("- %s\n", reason))
		}
		builder.WriteString("\n")
	}

	builder.WriteString("## Findings by attributable class\n\n")
	if len(verdict.Findings) == 0 {
		builder.WriteString("No differences were observed beyond the declared identity.\n\n")
	} else {
		builder.WriteString("| Class | Blocks release | Tolerated | Magnitude | Detail |\n")
		builder.WriteString("|---|---|---|---|---|\n")
		for _, finding := range verdict.Findings {
			blocking := "no"
			if finding.Blocking {
				blocking = "yes"
			}
			tolerated := "no"
			if finding.Tolerated {
				tolerated = "yes"
			}
			builder.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
				finding.Class, blocking, tolerated, formatMagnitude(finding.Magnitude), escapePipes(finding.Detail)))
		}
		builder.WriteString("\n")
	}

	builder.WriteString("## Adjacent-cause separation\n\n")
	builder.WriteString(fmt.Sprintf(
		"- Constant lag (phase class): %.3f samples full, %.3f at start, %.3f at mid, %.3f at end; limit %.3f samples.\n"+
			"- Lag drift (resampling/timebase class): %.3f samples; limit %.3f samples.\n"+
			"- Polarity inversion (phase class): %t.\n"+
			"- Residual after alignment (quantization/level/waveform class): max %.3f LSB, mean %.3f LSB over %d samples; limit %.3f LSB.\n"+
			"- Fitted scalar gain: %.6f dB.\n\n",
		verdict.Waveform.FullLagSamples, verdict.Waveform.StartLagSamples, verdict.Waveform.MidLagSamples, verdict.Waveform.EndLagSamples,
		PhaseToleranceSamples,
		verdict.Waveform.LagDriftSamples, LagDriftToleranceSamples,
		verdict.Waveform.PolarityInverted,
		verdict.Waveform.MaxResidualLSB, verdict.Waveform.MeanResidualLSB, verdict.Waveform.ResidualSampleCount,
		verdict.Waveform.QuantizationTolerance,
		verdict.Waveform.FittedGainDB,
	))
	builder.WriteString("A constant lag is classified as a phase shift. A lag that changes across the duration is " +
		"classified as a resampling or timebase change even when the declared sample rate is unchanged. " +
		"A residual that survives alignment is classified as a waveform change unless it stays inside the " +
		"quantization tolerance, in which case it is requantization noise. These classes are distinct, so " +
		"fixing one cause cannot be mistaken for fixing another.\n\n")

	if !verdict.ByteIdenticalToBaseline {
		builder.WriteString("## Why a numeric difference is not an automatic failure\n\n")
		builder.WriteString("The candidate does not reproduce the historical bytes. Under the rule set this is still " +
			"acceptable when: the structural format matches the captured targets; the waveform aligns to the " +
			"baseline with no phase offset, no timebase drift and no polarity inversion; the residual after " +
			"alignment stays inside the declared quantization tolerance; the measured peak meets the target; " +
			"loudness stays within continuity tolerance; no sample clips; and the declared selection and " +
			"processing independently reconstruct the downloaded bytes and the receipt agrees with both. " +
			"A difference that cannot be attributed to one of those classes is not waived for being small.\n\n")
	}

	builder.WriteString("## Reconstruction from the record\n\n")
	builder.WriteString(fmt.Sprintf("- Expected output frames from the declared spans: %d; downloaded frames: %d.\n",
		verdict.ReconstructionFrames, verdict.DownloadedFrames))
	builder.WriteString(fmt.Sprintf("- Downloaded digest: `%s`.\n", verdict.DownloadedDigest))
	if verdict.ReconstructionAvailable {
		builder.WriteString(fmt.Sprintf("- The reference rebuild matches the download within tolerance: %t.\n", verdict.ReconstructionMatches))
	} else {
		builder.WriteString("- The reference rebuild could not be produced from the record; see findings.\n")
	}
	builder.WriteString("The reference rebuild is produced from the source selection, the adopted processing conditions " +
		"and the record's own copy of the source bytes. It is not produced by the production renderer, so the " +
		"rebuild and the delivered bytes agreeing is an independent confirmation.\n")
	return builder.String()
}

func plainOutcome(outcome string) string {
	switch outcome {
	case OutcomeReleasePass:
		return "PASS - identical to the accepted delivery and all targets hold"
	case OutcomeCompatibleAccept:
		return "COMPATIBLE - differences are fully attributed to tolerated classes"
	case OutcomeBlockRelease:
		return "BLOCK RELEASE - an attributable difference exceeds tolerance"
	case OutcomeInsufficientEvidence:
		return "INSUFFICIENT EVIDENCE - the record cannot support a decision"
	default:
		return outcome
	}
}

func formatValue(value float64, unit string) string {
	if math.IsNaN(value) {
		return "n/a"
	}
	switch unit {
	case "boolean":
		if value != 0 {
			return "true"
		}
		return "false"
	case "dBFS", "dB":
		return fmt.Sprintf("%.3f", value)
	case "LSB":
		return fmt.Sprintf("%.3f", value)
	case "samples":
		return fmt.Sprintf("%.2f", value)
	case "frames":
		return fmt.Sprintf("%d", int64(value))
	case "Hz", "channels", "bits":
		return fmt.Sprintf("%d", int64(value))
	default:
		return fmt.Sprintf("%.4g", value)
	}
}

func formatMagnitude(value float64) string {
	if math.IsNaN(value) || value == 0 {
		return "-"
	}
	return fmt.Sprintf("%.4g", value)
}

func escapePipes(value string) string {
	return strings.ReplaceAll(value, "|", "\\|")
}
