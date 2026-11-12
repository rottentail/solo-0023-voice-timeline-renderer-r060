package auditdemo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"voice-timeline-renderer/internal/audit"
)

// Scenario is one rehearsed maintenance case. It declares:
//
//   - the intended cause,
//   - the outcome the rule set must produce,
//   - the finding class that proves the cause was identified,
//   - how to build the candidate and its evidence.
//
// Intended cause and measured class are separate fields on purpose: a
// scenario passes only when the rule identifies the same cause the
// transformation introduced.
type Scenario struct {
	Name         string
	Title        string
	Rationale    string
	Intent       string
	WantOutcome  string
	WantClass    string
	WantNotClass string
	Transforms   transformPlan
	LateDelivery bool
}

type transformPlan struct {
	// source edits applied to the production inputs before rendering
	SourceATransform scenarioTransform
	SourceBTransform scenarioTransform
	// post edits applied to the production output bytes
	PostTransform scenarioTransform
	// redeclared conditions: what the producer says was done. When empty the
	// baseline declaration is reused.
	DeclaredPhaseDelay   *float64
	DeclaredPolarity     *bool
	DeclaredClockRatio   *float64
	DeclaredQuantization *string
	OmitReceipt          bool
}

// Scenarios returns the full rehearsal set keyed by name.
func Scenarios() map[string]Scenario {
	result := map[string]Scenario{
		"identical-render": {
			Name:        "identical-render",
			Title:       "Reproduced historical render",
			Intent:      "no change",
			WantOutcome: audit.OutcomeReleasePass,
			WantClass:   "digitally_identical_waveform",
			Rationale:   "the re-render reproduces the accepted bytes exactly",
		},
		"requantize-round": {
			Name:        "requantize-round",
			Title:       "Re-render with the same settings",
			Intent:      "quantization",
			WantOutcome: audit.OutcomeCompatibleAccept,
			WantClass:   "quantization_noise",
			Transforms: transformPlan{
				PostTransform: scenarioTransform{ditherLSB: 0.6},
			},
			Rationale: "the only difference is one-LSB output rounding; the delivered signal contract is unchanged",
		},
		"requantize-truncate": {
			Name:        "requantize-truncate",
			Title:       "Re-render using truncation instead of rounding",
			Intent:      "quantization",
			WantOutcome: audit.OutcomeCompatibleAccept,
			WantClass:   "quantization_noise",
			Transforms: transformPlan{
				DeclaredQuantization: stringPointer(audit.QuantTruncateTowardZero),
				PostTransform:        scenarioTransform{truncate: true, ditherLSB: 0.8},
			},
			Rationale: "truncation stays inside the declared one-LSB quantization tolerance and is fully attributed",
		},
		"phase-offset": {
			Name:        "phase-offset",
			Title:       "Output delayed by a constant number of samples",
			Intent:      "phase",
			WantOutcome: audit.OutcomeBlockRelease,
			WantClass:   "phase_offset",
			Transforms: transformPlan{
				PostTransform:      scenarioTransform{frameshift: 3},
				DeclaredPhaseDelay: floatPointer(3),
			},
			Rationale: "a constant delay is a phase-class change and cannot be accepted as rounding noise",
		},
		"resample-drift": {
			Name:        "resample-drift",
			Title:       "Resampler ran at a slightly wrong clock",
			Intent:      "resampling",
			WantOutcome: audit.OutcomeBlockRelease,
			WantClass:   "resampling_timebase_change",
			Transforms: transformPlan{
				SourceBTransform: scenarioTransform{clockRatio: 1.0009},
			},
			Rationale: "lag changes across the duration, which is resampling-class and distinct from a constant phase offset",
		},
		"waveform-change": {
			Name:        "waveform-change",
			Title:       "Same length and level but a different signal",
			Intent:      "waveform",
			WantOutcome: audit.OutcomeBlockRelease,
			WantClass:   "waveform_residual_over_tolerance",
			Transforms: transformPlan{
				SourceATransform: scenarioTransform{addNoise: 0.02},
			},
			Rationale: "a residual beyond requantization is a real signal change, not a legal numeric difference",
		},
		"level-drift": {
			Name:        "level-drift",
			Title:       "Output level outside the declared peak tolerance",
			Intent:      "level",
			WantOutcome: audit.OutcomeBlockRelease,
			WantClass:   "peak_target_missed",
			Transforms: transformPlan{
				PostTransform: scenarioTransform{levelDB: -3},
			},
			Rationale: "the measured peak misses the captured target by more than the declared tolerance",
		},
		"polarity-inverted": {
			Name:        "polarity-inverted",
			Title:       "Output polarity reversed",
			Intent:      "phase",
			WantOutcome: audit.OutcomeBlockRelease,
			WantClass:   "polarity_inversion",
			Transforms: transformPlan{
				PostTransform:    scenarioTransform{polarityInvert: true},
				DeclaredPolarity: boolPointer(true),
			},
			Rationale: "polarity inversion is a phase-class change and is reconstructed from the declared condition",
		},
		"duration-drift": {
			Name:        "duration-drift",
			Title:       "Delivery stopped early and lost frames",
			Intent:      "resampling",
			WantOutcome: audit.OutcomeBlockRelease,
			WantClass:   "resampling_timebase_change",
			Transforms: transformPlan{
				PostTransform:      scenarioTransform{clockRatio: 0.995},
				DeclaredClockRatio: floatPointer(0.995),
			},
			Rationale: "a stretched delivery changes the timebase and cannot be accepted as requantization",
		},
		"receipt-missing": {
			Name:        "receipt-missing",
			Title:       "Producer omitted the receipt",
			Intent:      "evidence",
			WantOutcome: audit.OutcomeInsufficientEvidence,
			WantClass:   "receipt_missing",
			Transforms:  transformPlan{OmitReceipt: true},
			Rationale:   "without a receipt the delivered bytes cannot be reconciled with any declared result",
		},
		"stale-delivery": {
			Name:         "stale-delivery",
			Title:        "Third-party re-render arrived after the historical delivery",
			Intent:       "third-party",
			WantOutcome:  audit.OutcomeCompatibleAccept,
			WantClass:    "quantization_noise",
			LateDelivery: true,
			Transforms: transformPlan{
				PostTransform: scenarioTransform{ditherLSB: 0.6},
			},
			Rationale: "the late delivery is appended as its own candidate and decision; the earlier decision remains in the record",
		},
	}
	return result
}

// ScenarioNames returns a deterministic ordered list.
func ScenarioNames() []string {
	names := make([]string, 0, len(Scenarios()))
	for name := range Scenarios() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RunScenario records and audits one scenario under its own record ID.
// Re-running in an existing directory re-reads the recorded verdict rather
// than recomputing it, which is the immutability property being rehearsed.
func RunScenario(ctx context.Context, store *audit.Store, runner *Runner, delivery *Delivery, scenario Scenario, verbose bool) error {
	recordID := "record-" + scenario.Name
	recordDir, err := store.EnsureRecordDirectory(recordID)
	if err != nil {
		return err
	}
	existing := false
	if _, statErr := os.Stat(filepath.Join(recordDir, "record.json")); statErr == nil {
		existing = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if !existing {
		if _, err := store.Create(ctx, audit.NewRecord{
			RecordID:  recordID,
			Title:     scenario.Title,
			Actor:     "historical-producer",
			Targets:   delivery.Targets(),
			Baseline:  delivery.BaselineCandidate(),
			Artifacts: delivery.BaselineArtifacts(),
		}); err != nil {
			return err
		}
		if _, err := store.RecordDecision(ctx, recordID, audit.DecisionInput{
			Actor:       "release-reviewer",
			CandidateID: baselineRender,
			Rationale:   "historical delivery accepted; its bytes are the recorded byte-level promise",
			Historical:  true,
		}); err != nil {
			return err
		}
	}

	// The late delivery case submits a candidate whose declared production
	// time precedes the audit; the record keeps the historical decision above.
	candidate, candidateArtifacts, err := buildCandidate(ctx, runner, delivery, scenario)
	if err != nil {
		return err
	}
	if !existing {
		if _, _, err := store.Ingest(ctx, recordID, audit.IngestInput{Candidate: candidate, Artifacts: candidateArtifacts}); err != nil {
			return err
		}
	}
	verdict, err := store.Review(ctx, recordID, candidate.ID)
	if err != nil {
		return err
	}
	reportPath, err := store.WriteReport(ctx, recordID, candidate.ID, verdict)
	if err != nil {
		return err
	}
	if !existing {
		if _, err := store.RecordDecision(ctx, recordID, audit.DecisionInput{
			Actor:       "release-reviewer",
			CandidateID: candidate.ID,
			Rationale:   scenario.Rationale,
		}); err != nil {
			return err
		}
	}
	if _, err := store.VerifyChain(ctx, recordID); err != nil {
		return fmt.Errorf("audit chain failed to verify: %w", err)
	}
	if err := store.RecheckCandidate(ctx, recordID, baselineRender); err != nil {
		return err
	}

	status := "ok"
	if verdict.Outcome != scenario.WantOutcome {
		status = "MISMATCH expected " + scenario.WantOutcome
	}
	if !containsClass(verdict.Findings, scenario.WantClass) {
		status = "MISMATCH expected class " + scenario.WantClass
	}
	if scenario.WantNotClass != "" && containsClass(verdict.Findings, scenario.WantNotClass) {
		status = "MISMATCH unexpected class " + scenario.WantNotClass
	}
	fmt.Printf("%-22s %-24s %s\n", scenario.Name, verdict.Outcome, status)
	if verbose {
		fmt.Printf("  record=%s candidate=%s\n  report=%s\n", recordID, candidate.ID, reportPath)
		for _, finding := range verdict.Findings {
			if finding.Blocking || finding.Tolerated {
				fmt.Printf("  class=%-44s blocking=%-5t tolerated=%t magnitude=%.3f\n",
					finding.Class, finding.Blocking, finding.Tolerated, finding.Magnitude)
			}
		}
	}
	if status != "ok" {
		return fmt.Errorf("scenario %s %s", scenario.Name, status)
	}
	return nil
}

func containsClass(findings []audit.Finding, class string) bool {
	for _, finding := range findings {
		if finding.Class == class {
			return true
		}
	}
	return false
}

// Targets captures the historical promise exactly once, from the accepted
// delivery, in the record creation path.
func (delivery *Delivery) Targets() audit.FidelityTargets {
	return audit.FidelityTargets{
		SampleRate:               delivery.SampleRate,
		Channels:                 delivery.Channels,
		BitsPerSample:            16,
		TargetPeakDBFS:           productionTargetPeakDBFS,
		PeakToleranceDB:          0.25,
		LoudnessToleranceDB:      2.0,
		QuantizationToleranceLSB: 1.5,
		DurationToleranceMS:      1.0,
		HistoricalOutputDigest:   delivery.Digest,
		HistoricalOutputBytes:    int64(len(delivery.PCM)),
	}
}

func stringPointer(value string) *string  { return &value }
func boolPointer(value bool) *bool        { return &value }
func floatPointer(value float64) *float64 { return &value }

func join(parts ...string) string { return strings.Join(parts, "; ") }
