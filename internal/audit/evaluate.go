package audit

import (
	"context"
	"fmt"
	"math"
)

// Severity is the maintenance decision level attached to a finding class.
// The outcome is derived from this table and nothing else, so the rules are a
// single reviewable artifact rather than behaviour scattered across the code.
type Severity string

const (
	// SeverityInsufficient means the record cannot be independently rebuilt.
	// It is never a pass, and it is never reported as a release failure:
	// the reviewer must ask for the missing evidence instead.
	SeverityInsufficient Severity = "insufficient_evidence"
	// SeverityBlock means a cause is identified and its magnitude exceeds the
	// declared tolerance. Release is blocked.
	SeverityBlock Severity = "block_release"
	// SeverityTolerate means the difference belongs to a class this rule set
	// deliberately accepts, with a bounded magnitude.
	SeverityTolerate Severity = "tolerate"
)

// ruleEntry binds a finding class to its severity and a plain-language
// explanation. Every class the engine can emit must appear here; an unknown
// class is treated as blocking so a new finding cannot pass by accident.
type ruleEntry struct {
	Class    string
	Severity Severity
	Meaning  string
}

// ruleTable is the maintenance rule set. It is the authoritative list of
// attributable difference classes and how each one is decided.
var ruleTable = []ruleEntry{
	// --- Evidentiary classes: the choice cannot be rebuilt ----------------
	{"receipt_missing", SeverityInsufficient,
		"no receipt means the download cannot be reconciled with a declared result"},
	{"selection_source_missing", SeverityInsufficient,
		"the record does not carry the source bytes the span selected"},
	{"reconstruction_not_possible", SeverityInsufficient,
		"the declared selection or processing conditions cannot be rebuilt"},
	{"declared_processing_does_not_explain_download", SeverityInsufficient,
		"the rebuilt reference does not reproduce the downloaded bytes"},
	{"declared_format_does_not_match_download", SeverityInsufficient,
		"the declared format disagrees with the downloaded artifact"},
	{"reconstruction_frame_count_mismatch", SeverityInsufficient,
		"the declared spans do not predict the delivered frame count"},
	{"reconstruction_missing", SeverityInsufficient,
		"no reconstruction was recorded for this candidate"},
	{"evidence_incomplete", SeverityInsufficient,
		"candidate, baseline, or observation facts are missing"},
	{"waveform_not_comparable", SeverityInsufficient,
		"the two artifacts cannot be aligned, so no cause can be attributed"},
	{"timebase_not_measurable", SeverityInsufficient,
		"the payload is too short to measure drift, so the timebase is unverified"},

	// --- Integrity classes: the artifacts contradict each other -----------
	{"receipt_digest_mismatch", SeverityBlock,
		"the receipt names a digest that is not the delivered bytes"},
	{"receipt_frame_count_mismatch", SeverityBlock,
		"the receipt declares a frame count the download does not have"},
	{"receipt_peak_disagrees_with_download", SeverityBlock,
		"the receipt's measured peak is not the download's measured peak"},
	{"receipt_rms_disagrees_with_download", SeverityBlock,
		"the receipt's measured RMS is not the download's measured RMS"},
	{"receipt_spans_disagree_with_selection", SeverityBlock,
		"the receipt's span placement or source digests contradict the declaration"},
	{"receipt_normalization_target_mismatch", SeverityBlock,
		"the receipt's normalization target contradicts the declared condition"},
	{"selection_source_rate_mismatch", SeverityBlock,
		"the declared source rate contradicts the stored source"},
	{"selection_source_channels_mismatch", SeverityBlock,
		"the declared source channel count contradicts the stored source"},

	// --- Format and timing classes ----------------------------------------
	{"sample_rate_mismatch", SeverityBlock, "the delivered rate is not the target rate"},
	{"channel_layout_mismatch", SeverityBlock, "the delivered layout is not the target layout"},
	{"bit_width_mismatch", SeverityBlock, "the delivered sample width is not the target width"},
	{"frame_count_mismatch", SeverityBlock, "the delivered duration differs beyond tolerance"},
	{"frame_count_within_duration_tolerance", SeverityTolerate,
		"a one-frame rounding difference at a rate boundary stays inside the duration tolerance"},

	// --- Phase class -------------------------------------------------------
	{"phase_offset", SeverityBlock,
		"a constant delay changes alignment, which is a phase-class defect"},
	{"polarity_inversion", SeverityBlock,
		"a global sign flip changes phase by 180 degrees"},

	// --- Resampling class --------------------------------------------------
	{"resampling_timebase_change", SeverityBlock,
		"the lag grows across the duration, so the timebase changed"},

	// --- Level class -------------------------------------------------------
	{"peak_target_missed", SeverityBlock, "the measured peak misses the captured target"},
	{"loudness_drift", SeverityBlock, "loudness moved beyond the continuity tolerance"},
	{"clipping", SeverityBlock, "full-scale samples indicate hard clipping"},
	{"silent_output", SeverityBlock, "an all-silent output cannot meet a signal target"},

	// --- Quantization class ------------------------------------------------
	{"quantization_noise", SeverityTolerate,
		"aligned residual stays inside the declared quantization tolerance"},
	{"digitally_identical_waveform", SeverityTolerate,
		"aligned waveform is sample-identical"},

	// --- Waveform class ----------------------------------------------------
	{"waveform_residual_over_tolerance", SeverityBlock,
		"the aligned residual exceeds requantization, so the signal changed"},
}

func lookupRule(class string) (ruleEntry, bool) {
	for _, entry := range ruleTable {
		if entry.Class == class {
			return entry, true
		}
	}
	return ruleEntry{}, false
}

// RuleTable returns the authoritative class-to-severity mapping. It is
// exported so documentation and tests can assert the rule set directly.
func RuleTable() []ruleEntry {
	result := make([]ruleEntry, len(ruleTable))
	copy(result, ruleTable)
	return result
}

// SeverityFor returns the decision level for a finding class. An unknown
// class returns SeverityBlock with found=false; callers must treat that as
// blocking and flag the class as unclassified.
func SeverityFor(class string) (Severity, bool) {
	entry, ok := lookupRule(class)
	if !ok {
		return SeverityBlock, false
	}
	return entry.Severity, true
}

// EvaluateInput is everything the rule engine may consider. It deliberately
// receives stored facts only; the engine recomputes nothing from the
// production renderer.
type EvaluateInput struct {
	Candidate      *Candidate
	Baseline       *Candidate
	Targets        FidelityTargets
	Observation    *Observation
	BaselineObs    *Observation
	Waveform       WaveformComparison
	Structure      StructuralComparison
	Reconstruction *Reconstruction
	ReconFindings  []Finding
	ByteIdentical  bool
	RuleSetID      string
	DecidedAt      string
}

// Evaluate applies the versioned maintenance rules and returns an immutable
// verdict.
//
// The decision order is deliberate and is implemented by the rule table:
//
//  1. If any finding is blocking, the outcome is BLOCK_RELEASE. A definitive
//     defect outranks an explanatory gap: a real difference must not be
//     retried as a paperwork problem.
//  2. Otherwise, if any finding is evidentiary, the outcome is
//     INSUFFICIENT_EVIDENCE. The reviewer asks for the missing artifact rather
//     than guessing.
//  3. Otherwise, identical bytes are RELEASE_PASS.
//  4. Otherwise, fully attributed tolerated differences are
//     COMPATIBLE_ACCEPT.
//
// No difference is waived merely for being small, and no legal numeric
// difference is failed merely for not being byte-identical.
func Evaluate(ctx context.Context, input EvaluateInput) (Verdict, error) {
	if err := readContextErr(ctx); err != nil {
		return Verdict{}, err
	}
	verdict := Verdict{
		CandidateID:         input.CandidateID(),
		BaselineCandidateID: input.BaselineID(),
		RuleSetID:           input.RuleSetID,
		DecidedAt:           input.DecidedAt,
		Structural:          input.Structure,
		Waveform:            input.Waveform,
	}

	findings := make([]Finding, 0, 12+len(input.ReconFindings))
	findings = append(findings, input.ReconFindings...)
	checks := make([]CheckResult, 0, 12)

	// --- Evidence completeness ---------------------------------------------
	if input.Candidate == nil || input.Baseline == nil || input.Observation == nil || input.BaselineObs == nil {
		findings = append(findings, Finding{
			Class:      "evidence_incomplete",
			Attributes: []string{"insufficient"},
			Detail:     "candidate, baseline, or observation facts are missing",
		})
	}
	if input.Reconstruction == nil {
		findings = append(findings, Finding{
			Class:      "reconstruction_missing",
			Attributes: []string{"insufficient"},
			Detail:     "no independent reconstruction exists, so the choice cannot be rebuilt",
		})
	}

	// --- Historical byte identity ------------------------------------------
	byteIdentical := input.ByteIdentical
	verdict.ByteIdenticalToBaseline = byteIdentical
	checks = append(checks, CheckResult{
		Name:   "historical_byte_identity",
		Passed: byteIdentical,
		Unit:   "boolean",
		Details: func() string {
			if byteIdentical {
				return "downloaded PCM hash matches the recorded historical delivery digest"
			}
			return "download differs from the recorded historical delivery digest"
		}(),
	})

	// --- Structural rules --------------------------------------------------
	structureChecks, structureFindings := evaluateStructure(input)
	checks = append(checks, structureChecks...)
	findings = append(findings, structureFindings...)

	// --- Adjacent-cause classification ------------------------------------
	waveChecks, waveFindings := classifyWaveform(input)
	checks = append(checks, waveChecks...)
	findings = append(findings, waveFindings...)

	// --- Signal-quality rules ----------------------------------------------
	qualityChecks, qualityFindings := evaluateQuality(input)
	checks = append(checks, qualityChecks...)
	findings = append(findings, qualityFindings...)

	// --- Apply the authoritative severity table ---------------------------
	insufficient := false
	blocking := false
	for index := range findings {
		finding := &findings[index]
		severity, known := SeverityFor(finding.Class)
		if !known {
			// An unclassified difference is never waived. It blocks and is
			// marked so a maintainer adds it to the rule table deliberately.
			finding.Attributes = appendUnique(finding.Attributes, "unclassified")
			finding.Blocking = true
			finding.Tolerated = false
			blocking = true
			continue
		}
		finding.Blocking = severity == SeverityBlock
		finding.Tolerated = severity == SeverityTolerate
		switch severity {
		case SeverityInsufficient:
			insufficient = true
		case SeverityBlock:
			blocking = true
		}
	}
	verdict.Checks = checks
	verdict.Findings = findings

	switch {
	case blocking:
		// A definitively identified difference outranks an explanatory gap.
		// Reporting "insufficient evidence" here would let a real defect be
		// retried as a paperwork problem, so block first. The explanatory
		// findings remain in the verdict.
		verdict.Outcome = OutcomeBlockRelease
		verdict.Reasons = append(verdict.Reasons,
			"at least one attributable difference exceeds the declared tolerance")
	case insufficient:
		verdict.Outcome = OutcomeInsufficientEvidence
		verdict.Reasons = append(verdict.Reasons,
			"the record does not contain enough independently checkable evidence to rebuild the choice")
	case byteIdentical:
		verdict.Outcome = OutcomeReleasePass
		verdict.Reasons = append(verdict.Reasons,
			"downloaded bytes are identical to the accepted historical delivery and all signal targets hold")
	default:
		verdict.Outcome = OutcomeCompatibleAccept
		verdict.Reasons = append(verdict.Reasons,
			"every observed difference is attributed to a tolerated class and all signal targets hold")
	}

	// Attach the first attributable cause so the reviewer does not have to
	// scan findings to learn why the outcome is what it is.
	switch verdict.Outcome {
	case OutcomeBlockRelease:
		for _, finding := range findings {
			if finding.Blocking {
				verdict.Reasons = append(verdict.Reasons, finding.Class+": "+finding.Detail)
				break
			}
		}
	case OutcomeInsufficientEvidence:
		for _, finding := range findings {
			if severity, _ := SeverityFor(finding.Class); severity == SeverityInsufficient {
				verdict.Reasons = append(verdict.Reasons, finding.Class+": "+finding.Detail)
				break
			}
		}
	}
	return verdict, nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (input EvaluateInput) CandidateID() string {
	if input.Candidate == nil {
		return ""
	}
	return input.Candidate.ID
}

func (input EvaluateInput) BaselineID() string {
	if input.Baseline == nil {
		return ""
	}
	return input.Baseline.ID
}

func evaluateStructure(input EvaluateInput) ([]CheckResult, []Finding) {
	var checks []CheckResult
	var findings []Finding
	structure := input.Structure

	checks = append(checks, CheckResult{
		Name:    "sample_rate_matches_target",
		Passed:  structure.RateMatches,
		Value:   float64(structure.CandidateSampleRate),
		Limit:   float64(structure.BaselineSampleRate),
		Unit:    "Hz",
		Details: fmt.Sprintf("baseline %d Hz, download %d Hz", structure.BaselineSampleRate, structure.CandidateSampleRate),
	})
	if !structure.RateMatches {
		findings = append(findings, Finding{
			Class:      "sample_rate_mismatch",
			Attributes: []string{"format", "resampling"},
			Detail:     fmt.Sprintf("output sample rate is %d Hz, declared target is %d Hz", structure.CandidateSampleRate, structure.BaselineSampleRate),
		})
	}
	checks = append(checks, CheckResult{
		Name:    "channel_layout_matches_target",
		Passed:  structure.ChannelsMatch,
		Value:   float64(structure.CandidateChannels),
		Limit:   float64(structure.BaselineChannels),
		Unit:    "channels",
		Details: fmt.Sprintf("baseline %d channels, download %d channels", structure.BaselineChannels, structure.CandidateChannels),
	})
	if !structure.ChannelsMatch {
		findings = append(findings, Finding{
			Class:      "channel_layout_mismatch",
			Attributes: []string{"format", "channel_mapping"},
			Detail:     fmt.Sprintf("download has %d channels, declared target is %d", structure.CandidateChannels, structure.BaselineChannels),
		})
	}
	checks = append(checks, CheckResult{
		Name:    "bit_width_matches_target",
		Passed:  structure.BitsMatch,
		Value:   float64(structure.CandidateBits),
		Limit:   float64(structure.BaselineBits),
		Unit:    "bits",
		Details: fmt.Sprintf("baseline %d bits, download %d bits", structure.BaselineBits, structure.CandidateBits),
	})
	if !structure.BitsMatch {
		findings = append(findings, Finding{
			Class:      "bit_width_mismatch",
			Attributes: []string{"format", "quantization"},
			Detail:     fmt.Sprintf("download uses %d-bit samples, target is %d-bit", structure.CandidateBits, structure.BaselineBits),
		})
	}
	checks = append(checks, CheckResult{
		Name:    "frame_count_matches_target",
		Passed:  structure.FramesMatch,
		Value:   float64(structure.CandidateFrames),
		Limit:   float64(structure.BaselineFrames),
		Unit:    "frames",
		Details: fmt.Sprintf("download %d frames, baseline %d frames, delta %.3f ms", structure.CandidateFrames, structure.BaselineFrames, structure.DurationDeltaMS),
	})
	if !structure.FramesMatch {
		if structure.DurationDeltaMS > input.Targets.DurationToleranceMS {
			// A length change and a lag that grows across the duration are the
			// same timebase class; the timebase finding is reported once so a
			// reviewer sees one cause, not two names for it.
			if input.Waveform.LagDriftSamples <= LagDriftToleranceSamples {
				findings = append(findings, Finding{
					Class:      "frame_count_mismatch",
					Attributes: []string{"timebase", "selection"},
					Detail:     fmt.Sprintf("download has %d frames, baseline has %d frames (%.3f ms)", structure.CandidateFrames, structure.BaselineFrames, structure.DurationDeltaMS),
					Magnitude:  float64(structure.CandidateFrames - structure.BaselineFrames),
				})
			}
		} else {
			findings = append(findings, Finding{
				Class:      "frame_count_within_duration_tolerance",
				Attributes: []string{"timebase"},
				Detail:     fmt.Sprintf("download has %d frames, baseline has %d frames (%.3f ms, within %.2f ms)", structure.CandidateFrames, structure.BaselineFrames, structure.DurationDeltaMS, input.Targets.DurationToleranceMS),
				Magnitude:  structure.DurationDeltaMS,
			})
		}
	}
	return checks, findings
}

func classifyWaveform(input EvaluateInput) ([]CheckResult, []Finding) {
	wave := input.Waveform
	if !wave.Comparable {
		return []CheckResult{{
			Name:    "waveform_comparable",
			Passed:  false,
			Unit:    "boolean",
			Details: wave.Reason,
		}}, []Finding{{
			Class:      "waveform_not_comparable",
			Attributes: []string{"insufficient"},
			Detail:     wave.Reason,
		}}
	}

	var checks []CheckResult
	var findings []Finding
	constantLag := math.Abs(wave.FullLagSamples)

	checks = append(checks, CheckResult{
		Name:    "phase_alignment",
		Passed:  constantLag <= PhaseToleranceSamples,
		Value:   constantLag,
		Limit:   PhaseToleranceSamples,
		Unit:    "samples",
		Details: fmt.Sprintf("constant lag %.2f samples; start %.2f, mid %.2f, end %.2f", wave.FullLagSamples, wave.StartLagSamples, wave.MidLagSamples, wave.EndLagSamples),
	})
	checks = append(checks, CheckResult{
		Name:    "timebase_stability",
		Passed:  wave.SegmentLagsAvailable && wave.LagDriftSamples <= LagDriftToleranceSamples,
		Value:   wave.LagDriftSamples,
		Limit:   LagDriftToleranceSamples,
		Unit:    "samples",
		Details: "lag drift across the duration separates a resampling/timebase change from a constant phase shift",
	})
	if !wave.SegmentLagsAvailable {
		// Claiming "no drift" when drift cannot be measured would be a silent
		// pass. The timebase is unverified, so the record is insufficient.
		findings = append(findings, Finding{
			Class:      "timebase_not_measurable",
			Attributes: []string{"insufficient", "resampling"},
			Detail:     "the payload is too short to measure lag in multiple windows, so timebase drift cannot be ruled out",
		})
	}
	checks = append(checks, CheckResult{
		Name:    "polarity_preserved",
		Passed:  !wave.PolarityInverted,
		Unit:    "boolean",
		Details: "a global sign flip is a phase-class change, not level or quantization",
	})

	if wave.PolarityInverted {
		findings = append(findings, Finding{
			Class:      "polarity_inversion",
			Attributes: []string{"phase", "processing"},
			Detail:     "waveform is inverted relative to the baseline",
		})
	}
	timedrift := wave.LagDriftSamples > LagDriftToleranceSamples
	if timedrift {
		findings = append(findings, Finding{
			Class:      "resampling_timebase_change",
			Attributes: []string{"resampling", "processing"},
			Detail: fmt.Sprintf(
				"lag changes by %.2f samples across the duration although the declared rate is unchanged, which attributes the difference to resampling rather than a constant phase shift (constant lag %.2f)",
				wave.LagDriftSamples, wave.FullLagSamples,
			),
			Magnitude: wave.LagDriftSamples,
		})
	} else if constantLag > PhaseToleranceSamples {
		// Only report a phase offset when the lag is constant. A growing lag is
		// a different cause and gets its own finding above.
		findings = append(findings, Finding{
			Class:      "phase_offset",
			Attributes: []string{"phase", "processing"},
			Detail:     fmt.Sprintf("constant lag of %.2f samples exceeds the %.2f sample phase tolerance", wave.FullLagSamples, PhaseToleranceSamples),
			Magnitude:  constantLag,
		})
	}

	quantizationTolerance := input.Targets.QuantizationToleranceLSB
	checks = append(checks, CheckResult{
		Name:    "quantization_residual",
		Passed:  wave.MaxResidualLSB <= quantizationTolerance,
		Value:   wave.MaxResidualLSB,
		Limit:   quantizationTolerance,
		Unit:    "LSB",
		Details: fmt.Sprintf("max %.2f LSB, mean %.2f LSB, %d of %d samples over tolerance", wave.MaxResidualLSB, wave.MeanResidualLSB, wave.ResidualOverToleranceCount, wave.ResidualSampleCount),
	})
	checks = append(checks, CheckResult{
		Name:    "level_residual",
		Passed:  math.Abs(wave.FittedGainDB) <= input.Targets.PeakToleranceDB,
		Value:   wave.FittedGainDB,
		Limit:   input.Targets.PeakToleranceDB,
		Unit:    "dB",
		Details: "a single fitted scalar gain explains level differences; a gain change is level-class, not quantization-class",
	})

	switch {
	case !wave.ResidualComparable:
		// Frame lengths differ, so a residual over a truncated prefix cannot
		// be attributed to quantization or waveform content. The timebase
		// findings carry the block.
	case timedrift:
		// A timebase change drifts every sample against the reference, so the
		// residual is dominated by that drift. Naming it quantization or
		// waveform content would identify the wrong cause.
	case wave.MaxResidualLSB > quantizationTolerance:
		levelBlame := math.Abs(wave.FittedGainDB) > input.Targets.PeakToleranceDB
		attributes := []string{"waveform", "processing"}
		if levelBlame {
			attributes = []string{"level", "processing"}
		}
		findings = append(findings, Finding{
			Class:      "waveform_residual_over_tolerance",
			Attributes: attributes,
			Detail: fmt.Sprintf(
				"aligned residual reaches %.2f LSB with a fitted gain of %.3f dB: %s",
				wave.MaxResidualLSB, wave.FittedGainDB,
				func() string {
					if levelBlame {
						return "a level change beyond the declared peak tolerance, not requantization"
					}
					return "a waveform change beyond requantization"
				}(),
			),
			Magnitude: wave.MaxResidualLSB,
		})
	case wave.MaxResidualLSB > 0:
		findings = append(findings, Finding{
			Class:      "quantization_noise",
			Attributes: []string{"quantization"},
			Detail: fmt.Sprintf(
				"aligned residual stays within %.2f LSB (max %.2f, mean %.2f over %d samples) after removing the fitted level difference, attributable to requantization only",
				quantizationTolerance, wave.MaxResidualLSB, wave.MeanResidualLSB, wave.ResidualSampleCount,
			),
			Magnitude: wave.MaxResidualLSB,
		})
	default:
		findings = append(findings, Finding{
			Class:      "digitally_identical_waveform",
			Attributes: []string{"identity"},
			Detail:     "aligned waveform is sample-identical",
		})
	}
	return checks, findings
}

func evaluateQuality(input EvaluateInput) ([]CheckResult, []Finding) {
	observation := input.Observation
	var checks []CheckResult
	var findings []Finding

	peakError := math.Abs(observation.PeakDBFS - input.Targets.TargetPeakDBFS)
	checks = append(checks, CheckResult{
		Name:    "peak_target",
		Passed:  peakError <= input.Targets.PeakToleranceDB,
		Value:   observation.PeakDBFS,
		Limit:   input.Targets.TargetPeakDBFS,
		Unit:    "dBFS",
		Details: fmt.Sprintf("measured %.3f dBFS against target %.3f dBFS with tolerance %.2f dB", observation.PeakDBFS, input.Targets.TargetPeakDBFS, input.Targets.PeakToleranceDB),
	})
	if peakError > input.Targets.PeakToleranceDB {
		findings = append(findings, Finding{
			Class:      "peak_target_missed",
			Attributes: []string{"level", "normalization"},
			Detail:     fmt.Sprintf("measured peak %.3f dBFS misses the %.3f dBFS target by %.3f dB", observation.PeakDBFS, input.Targets.TargetPeakDBFS, peakError),
			Magnitude:  peakError,
		})
	}

	loudnessError := math.Abs(observation.RMSDBFS - input.BaselineObs.RMSDBFS)
	checks = append(checks, CheckResult{
		Name:    "loudness_continuity",
		Passed:  loudnessError <= input.Targets.LoudnessToleranceDB,
		Value:   loudnessError,
		Limit:   input.Targets.LoudnessToleranceDB,
		Unit:    "dB",
		Details: fmt.Sprintf("RMS %.3f dBFS versus baseline %.3f dBFS", observation.RMSDBFS, input.BaselineObs.RMSDBFS),
	})
	if loudnessError > input.Targets.LoudnessToleranceDB {
		findings = append(findings, Finding{
			Class:      "loudness_drift",
			Attributes: []string{"level", "processing"},
			Detail:     fmt.Sprintf("RMS changed by %.3f dB against a %.2f dB continuity tolerance", loudnessError, input.Targets.LoudnessToleranceDB),
			Magnitude:  loudnessError,
		})
	}

	checks = append(checks, CheckResult{
		Name:    "no_clipping",
		Passed:  observation.ClippedSampleCount == 0,
		Value:   float64(observation.ClippedSampleCount),
		Limit:   0,
		Unit:    "samples",
		Details: "full-scale samples indicate clipping and are not a normal numeric difference",
	})
	if observation.ClippedSampleCount > 0 {
		findings = append(findings, Finding{
			Class:      "clipping",
			Attributes: []string{"level", "normalization"},
			Detail:     fmt.Sprintf("%d samples sit at full scale", observation.ClippedSampleCount),
			Magnitude:  float64(observation.ClippedSampleCount),
		})
	}

	checks = append(checks, CheckResult{
		Name:    "signal_present",
		Passed:  observation.ActiveFrameCount > 0,
		Value:   float64(observation.ActiveFrameCount),
		Limit:   1,
		Unit:    "frames",
		Details: "an all-silent output cannot satisfy a signal-quality target",
	})
	if observation.ActiveFrameCount == 0 {
		findings = append(findings, Finding{
			Class:      "silent_output",
			Attributes: []string{"signal", "processing"},
			Detail:     "output contains no active frames",
		})
	}
	return checks, findings
}

// timebaseDominant reports whether a timebase change explains the residual.
func timebaseDominant(wave WaveformComparison, _ float64) bool {
	return wave.LagDriftSamples > LagDriftToleranceSamples
}
