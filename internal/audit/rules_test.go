package audit

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// sineClip builds an interleaved review signal at one rate.
func sineClip(rate, frames, channels int, frequency, amplitude float64) []float64 {
	samples := make([]float64, frames*channels)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(rate)
		value := amplitude * math.Sin(2*math.Pi*frequency*seconds)
		if channels == 1 {
			samples[frame] = value
			continue
		}
		samples[frame*2] = value
		samples[frame*2+1] = 0.8 * value
	}
	return samples
}

func pcmFromSamples(rate, channels int, samples []float64, method string) *PCM {
	payload, err := EncodePCM16Wave(rate, channels, Quantize16(samples, method))
	if err != nil {
		panic(err)
	}
	decoded, err := DecodePCM16Wave(payload)
	if err != nil {
		panic(err)
	}
	return decoded
}

func declaredCandidate(id string, selection Selection, gain float64, post PostProcessing) *Candidate {
	return &Candidate{
		ID:               id,
		Role:             "candidate",
		CreatedAt:        "2026-02-01T00:00:00Z",
		ProcessorVersion: "test",
		TimelineID:       "test-timeline",
		SampleRate:       selection.SourceSampleRate,
		OutputChannels:   selection.SourceChannels,
		Post:             post,
		Spans: []CandidateSpan{{
			Index:      0,
			Selection:  selection,
			Processing: SpanProcessing{GainDB: gain, ResampleMethod: ResampleLinear, ChannelMapping: MappingUnchanged},
		}},
	}
}

func baseTargets(digest string) FidelityTargets {
	return FidelityTargets{
		SampleRate:               48000,
		Channels:                 1,
		BitsPerSample:            16,
		TargetPeakDBFS:           -1,
		PeakToleranceDB:          0.25,
		LoudnessToleranceDB:      2,
		QuantizationToleranceLSB: 1.5,
		DurationToleranceMS:      1,
		HistoricalOutputDigest:   digest,
	}
}

// TestSeverityTableIsComplete asserts that every finding class the engine can
// emit is present in the authoritative table. An unlisted class blocks, so a
// missing entry is a real defect rather than a silent waiver.
func TestSeverityTableIsComplete(t *testing.T) {
	expected := map[string]Severity{
		"receipt_missing": SeverityInsufficient,
		"declared_processing_does_not_explain_download": SeverityInsufficient,
		"reconstruction_not_possible":                   SeverityInsufficient,
		"selection_source_missing":                      SeverityInsufficient,
		"peak_target_missed":                            SeverityBlock,
		"phase_offset":                                  SeverityBlock,
		"polarity_inversion":                            SeverityBlock,
		"resampling_timebase_change":                    SeverityBlock,
		"waveform_residual_over_tolerance":              SeverityBlock,
		"clipping":                                      SeverityBlock,
		"quantization_noise":                            SeverityTolerate,
		"digitally_identical_waveform":                  SeverityTolerate,
		"frame_count_within_duration_tolerance":         SeverityTolerate,
	}
	for class, want := range expected {
		got, ok := SeverityFor(class)
		if !ok {
			t.Errorf("class %q is missing from the rule table", class)
			continue
		}
		if got != want {
			t.Errorf("class %q has severity %q, want %q", class, got, want)
		}
	}
	if _, ok := SeverityFor("unlisted_class"); ok {
		t.Fatal("an unlisted class must not be known")
	}
}

// TestUnknownClassBlocks asserts that an unclassified finding blocks release.
func TestUnknownClassBlocks(t *testing.T) {
	baseline := pcmFromSamples(48000, 1, sineClip(48000, 4800, 1, 440, 0.4), QuantRoundHalfAway)
	candidate := pcmFromSamples(48000, 1, sineClip(48000, 4800, 1, 440, 0.4), QuantRoundHalfAway)
	targets := baseTargets(DigestBytes(mustWAVFromPCM(t, candidate)))
	verdict, err := Evaluate(context.Background(), EvaluateInput{
		Candidate:      &Candidate{ID: "c"},
		Baseline:       &Candidate{ID: "b"},
		Targets:        targets,
		Observation:    mustObservation(t, candidate),
		BaselineObs:    mustObservation(t, baseline),
		Waveform:       WaveformComparison{Comparable: true},
		Structure:      CompareStructure(baseline, candidate),
		Reconstruction: &Reconstruction{Available: true},
		ReconFindings: []Finding{{
			Class:  "never_classified",
			Detail: "a maintainer forgot to add this class",
		}},
		ByteIdentical: true,
		RuleSetID:     RuleSetID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Outcome != OutcomeBlockRelease {
		t.Fatalf("unclassified finding produced %q, want %q", verdict.Outcome, OutcomeBlockRelease)
	}
}

func TestPhaseOffsetIsNotConfusedWithResampling(t *testing.T) {
	base := sineClip(48000, 12000, 1, 440, 0.5)
	baseline := pcmFromSamples(48000, 1, base, QuantRoundHalfAway)

	// Constant three-sample delay.
	delayed := shiftSignal(base, 3)
	phase := pcmFromSamples(48000, 1, delayed, QuantRoundHalfAway)

	// Timebase change: same nominal rate, slight clock stretch, same length.
	stretched := stretchSignal(base, 1.0009)
	resampled := pcmFromSamples(48000, 1, stretched, QuantRoundHalfAway)

	phaseComparison, err := CompareWaveforms(context.Background(), baseline, phase, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(phaseComparison.FullLagSamples) < 2.5 {
		t.Fatalf("constant delay measured as lag %.2f, want about 3", phaseComparison.FullLagSamples)
	}
	if phaseComparison.LagDriftSamples > LagDriftToleranceSamples {
		t.Fatalf("constant delay was classified as timebase drift %.3f", phaseComparison.LagDriftSamples)
	}

	driftComparison, err := CompareWaveforms(context.Background(), baseline, resampled, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if driftComparison.LagDriftSamples <= LagDriftToleranceSamples {
		t.Fatalf("clock stretch was not detected as lag drift (%.3f)", driftComparison.LagDriftSamples)
	}
	if driftComparison.LagDriftSamples < 4*LagDriftToleranceSamples {
		t.Fatalf("drift magnitude %.3f is too close to the noise floor to be a robust distinction", driftComparison.LagDriftSamples)
	}
}

func TestQuantizationIsNotConfusedWithWaveformChange(t *testing.T) {
	base := sineClip(48000, 12000, 1, 440, 0.5)
	baseline := pcmFromSamples(48000, 1, base, QuantRoundHalfAway)

	// Legal numeric difference: a sub-LSB dither only.
	dither := make([]float64, len(base))
	copy(dither, base)
	for index := range dither {
		value := math.Sin(float64(index)*12.9898) * 43758.5453
		fraction := value - math.Floor(value)
		dither[index] += 0.6 * (2*fraction - 1) / 32767
	}
	requantized := pcmFromSamples(48000, 1, dither, QuantRoundHalfAway)

	// Real waveform change at a similar magnitude of visible difference.
	changed := make([]float64, len(base))
	copy(changed, base)
	for index := range changed {
		changed[index] += 0.0015 * math.Sin(float64(index)*1.6180339887)
	}
	waveformChanged := pcmFromSamples(48000, 1, changed, QuantRoundHalfAway)

	legalComparison, err := CompareWaveforms(context.Background(), baseline, requantized, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if legalComparison.MaxResidualLSB > 1.5 {
		t.Fatalf("sub-LSB dither residual %.2f exceeded quantization tolerance", legalComparison.MaxResidualLSB)
	}

	illegalComparison, err := CompareWaveforms(context.Background(), baseline, waveformChanged, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if illegalComparison.MaxResidualLSB <= 1.5 {
		t.Fatalf("real waveform change residual %.2f was not distinguishable from requantization", illegalComparison.MaxResidualLSB)
	}
	if illegalComparison.LagDriftSamples > LagDriftToleranceSamples {
		t.Fatalf("same-length waveform change should not look like timebase drift")
	}
}

func TestPolarityInversionIsPhaseClass(t *testing.T) {
	base := sineClip(48000, 8000, 1, 440, 0.5)
	inverted := make([]float64, len(base))
	for index := range base {
		inverted[index] = -base[index]
	}
	baseline := pcmFromSamples(48000, 1, base, QuantRoundHalfAway)
	candidate := pcmFromSamples(48000, 1, inverted, QuantRoundHalfAway)
	comparison, err := CompareWaveforms(context.Background(), baseline, candidate, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if !comparison.PolarityInverted {
		t.Fatal("an inverted delivery was not detected as polarity inversion")
	}
	if comparison.MaxResidualLSB > 1.5 {
		t.Fatalf("inverted delivery residual %.2f should be clean after canonicalization", comparison.MaxResidualLSB)
	}
	if comparison.FullLagSamples != 0 && math.Abs(comparison.FullLagSamples) > PhaseToleranceSamples {
		t.Fatalf("inverted delivery reported a phase offset of %.2f", comparison.FullLagSamples)
	}
}

func TestRebuildReproducesDeclaredProcessing(t *testing.T) {
	source := pcmFromSamples(44100, 2, sineClip(44100, 4410, 2, 330, 0.4), QuantRoundHalfAway)
	downloaded := pcmFromSamples(48000, 1, sineClip(48000, 4800, 1, 330, 0.4), QuantRoundHalfAway)

	candidate := &Candidate{
		ID:             "c1",
		SampleRate:     48000,
		OutputChannels: 1,
		Post:           PostProcessing{Quantization: QuantRoundHalfAway},
		Spans: []CandidateSpan{{
			Index: 0,
			Selection: Selection{
				SourceID:         "s",
				SourceStartFrame: 0,
				SourceEndFrame:   4410,
				SourceSampleRate: 44100,
				SourceChannels:   2,
			},
			Processing: SpanProcessing{GainDB: 0, ResampleMethod: ResampleLinear, ChannelMapping: MappingStereoToMono},
		}},
	}
	reconstruction, findings, err := Rebuild(context.Background(), RebuildInput{
		Candidate:        candidate,
		Sources:          map[string]*PCM{"s": source},
		Downloaded:       downloaded,
		DownloadedDigest: DigestBytes(mustWAVFromPCM(t, downloaded)),
		Receipt:          &ReceiptEvidence{Present: false},
		Targets:          baseTargets(""),
		RuleSetID:        RuleSetID,
	})
	if err != nil {
		t.Fatalf("rebuild returned error: %v", err)
	}
	if !reconstruction.Available {
		t.Fatalf("reconstruction was unavailable: %s", reconstruction.Reason)
	}
	if reconstruction.ExpectedFrames != downloaded.FrameCount {
		t.Fatalf("declared processing predicts %d frames, download has %d", reconstruction.ExpectedFrames, downloaded.FrameCount)
	}
	// Whether the record explains the download depends on the test signal, so
	// only the invariant "a mismatch always carries a finding" is asserted.
	if !reconstruction.MatchesDownloaded {
		found := false
		for _, finding := range findings {
			if finding.Class == "declared_processing_does_not_explain_download" {
				found = true
			}
		}
		if !found {
			t.Fatal("a record that cannot rebuild the download must produce an explainability finding")
		}
	}
}

func TestRebuildRejectsOutOfRangeSelection(t *testing.T) {
	source := pcmFromSamples(48000, 1, sineClip(48000, 480, 1, 440, 0.4), QuantRoundHalfAway)
	candidate := &Candidate{
		ID:             "c2",
		SampleRate:     48000,
		OutputChannels: 1,
		Post:           PostProcessing{Quantization: QuantRoundHalfAway},
		Spans: []CandidateSpan{{
			Selection: Selection{SourceID: "s", SourceStartFrame: 0, SourceEndFrame: 999999},
		}},
	}
	_, _, err := Rebuild(context.Background(), RebuildInput{
		Candidate:  candidate,
		Sources:    map[string]*PCM{"s": source},
		Downloaded: source,
		Targets:    baseTargets(""),
		RuleSetID:  RuleSetID,
	})
	if err == nil {
		t.Fatal("an out-of-range selection must not rebuild")
	}
	if _, ok := err.(*rebuildError); !ok {
		t.Fatalf("out-of-range selection returned %T, want a rebuildError", err)
	}
}

func TestStoreRefusesToRewriteCandidate(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	baseline := Candidate{ID: "baseline_01", Spans: []CandidateSpan{{Selection: Selection{SourceID: "s", SourceEndFrame: 10}}}}
	if _, err := store.Create(context.Background(), NewRecord{
		RecordID: "record_01",
		Title:    "immutability rehearsal",
		Actor:    "producer",
		Targets:  baseTargets(""),
		Baseline: baseline,
	}); err != nil {
		t.Fatal(err)
	}
	// Re-creating the same record identifier must fail.
	if _, err := store.Create(context.Background(), NewRecord{
		RecordID: "record_01", Title: "again", Actor: "producer", Targets: baseTargets(""), Baseline: baseline,
	}); err == nil {
		t.Fatal("recreating an existing record must be refused")
	}
	// Appending a candidate with an existing identifier must fail.
	if _, err := store.AddCandidate(context.Background(), "record_01", Candidate{ID: "baseline_01"}, nil); err == nil {
		t.Fatal("rewriting an existing candidate must be refused")
	}
}

func TestEvidenceTamperingIsDetected(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	pcm := pcmFromSamples(48000, 1, sineClip(48000, 960, 1, 440, 0.4), QuantRoundHalfAway)
	wav := mustWAVFromPCM(t, pcm)
	candidate := Candidate{
		ID: "baseline_01",
		Spans: []CandidateSpan{{
			Selection: Selection{SourceID: "s", SourceEndFrame: 960, SourceSampleRate: 48000, SourceChannels: 1},
		}},
	}
	if _, err := store.Create(context.Background(), NewRecord{
		RecordID:  "record_02",
		Title:     "tamper rehearsal",
		Actor:     "producer",
		Targets:   baseTargets(DigestBytes(wav)),
		Baseline:  candidate,
		Artifacts: []Artifact{{Kind: EvidenceDownloadedPCM, Subject: "output", Extension: "wav", Bytes: wav}},
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCandidate(context.Background(), "record_02", "baseline_01")
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok := ListedEvidence(loaded, EvidenceDownloadedPCM, "")
	if !ok {
		t.Fatal("stored evidence is missing")
	}
	recordDir, err := store.recordDir("record_02")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(recordDir, "candidates", "baseline_01", evidence.RelativePath)
	if err := os.WriteFile(path, append(wav[:len(wav)-2], 0xFF, 0xFF), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadEvidence(context.Background(), "record_02", "baseline_01", evidence); err == nil {
		t.Fatal("tampered evidence was accepted")
	}
}

func TestDecisionIsAppendOnly(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{ID: "baseline_01", Spans: []CandidateSpan{{Selection: Selection{SourceID: "s", SourceEndFrame: 10}}}}
	if _, err := store.Create(context.Background(), NewRecord{
		RecordID: "record_03", Title: "append-only", Actor: "producer", Targets: baseTargets(""), Baseline: candidate,
	}); err != nil {
		t.Fatal(err)
	}
	first, err := store.RecordDecision(context.Background(), "record_03", DecisionInput{
		Actor: "reviewer", CandidateID: "baseline_01", Rationale: "historical acceptance", Historical: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != OutcomeHistoricalAccept {
		t.Fatalf("historical decision outcome %q", first.Outcome)
	}
	second, err := store.RecordDecision(context.Background(), "record_03", DecisionInput{
		Actor: "reviewer", CandidateID: "baseline_01", Rationale: "still accepted", Historical: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := store.LoadDecisions(context.Background(), "record_03")
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 {
		t.Fatalf("expected two decisions, found %d", len(decisions))
	}
	if decisions[0].Sequence != 1 || decisions[0].Rationale != "historical acceptance" {
		t.Fatal("the first decision was modified")
	}
	if decisions[1].Sequence != 2 || second.Outcome != OutcomeHistoricalAccept {
		t.Fatal("the second decision was not appended")
	}
}

// receiptJSON builds the production receipt shape for synthetic evidence so
// that a test focuses on the rule under examination rather than on a
// deliberately absent receipt (which is itself the insufficient-evidence case
// covered elsewhere).
func receiptJSON(t *testing.T, candidate Candidate, pcm *PCM, digest string) []byte {
	t.Helper()
	observation := mustObservation(t, pcm)
	spans := make([]map[string]any, 0, len(candidate.Spans))
	for _, span := range candidate.Spans {
		spans = append(spans, map[string]any{
			"index":              span.Index,
			"source_id":          span.Selection.SourceID,
			"source_digest":      span.Selection.SourceDigest,
			"source_start_frame": span.Selection.SourceStartFrame,
			"source_end_frame":   span.Selection.SourceEndFrame,
			"output_start_frame": 0,
			"output_end_frame":   pcm.FrameCount,
			"output_frames":      pcm.FrameCount,
			"crossfade_frames":   span.Processing.CrossfadeFrames,
			"gain_db":            span.Processing.GainDB,
		})
	}
	payload := map[string]any{
		"schema_version": 1,
		"render_id":      candidate.ID,
		"timeline_id":    candidate.TimelineID,
		"output": map[string]any{
			"digest":          digest,
			"byte_count":      pcm.ByteCount,
			"sample_rate":     pcm.SampleRate,
			"channels":        pcm.Channels,
			"bits_per_sample": pcm.BitsPerSample,
			"frame_count":     pcm.FrameCount,
			"peak_dbfs":       observation.PeakDBFS,
			"rms_dbfs":        observation.RMSDBFS,
		},
		"normalization": map[string]any{
			"requested_peak_dbfs": candidate.Post.NormalizationTargetDBFS,
			"applied_gain_db":     candidate.Post.AppliedGainDB,
			"output_peak_dbfs":    observation.PeakDBFS,
		},
		"spans":      spans,
		"warnings":   []string{},
		"created_at": "2026-01-10T12:00:00Z",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustObservation(t *testing.T, pcm *PCM) *Observation {
	t.Helper()
	observation, err := ObservePCM(context.Background(), "", DigestBytes(mustWAVFromPCM(t, pcm)), pcm, "")
	if err != nil {
		t.Fatal(err)
	}
	return &observation
}

func mustWAVFromPCM(t *testing.T, pcm *PCM) []byte {
	t.Helper()
	payload, err := EncodePCM16Wave(pcm.SampleRate, pcm.Channels, pcm.Samples)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func shiftSignal(samples []float64, frames float64) []float64 {
	result := make([]float64, len(samples))
	for index := range result {
		position := float64(index) - frames
		left := int(math.Floor(position))
		weight := position - float64(left)
		first, second := 0.0, 0.0
		if left >= 0 && left < len(samples) {
			first = samples[left]
		}
		if left+1 >= 0 && left+1 < len(samples) {
			second = samples[left+1]
		}
		result[index] = first + (second-first)*weight
	}
	return result
}

func stretchSignal(samples []float64, ratio float64) []float64 {
	result := make([]float64, len(samples))
	for index := range result {
		position := float64(index) / ratio
		left := int(math.Floor(position))
		if left >= len(samples)-1 {
			left = len(samples) - 1
		}
		right := left + 1
		if right >= len(samples) {
			right = len(samples) - 1
		}
		weight := position - float64(left)
		result[index] = samples[left] + (samples[right]-samples[left])*weight
	}
	return result
}

// TestLateDeliveryPreservesHistoricalConclusion is the core immutability
// rehearsal: a third-party candidate arriving after the historical delivery
// must be appended, and the original historical decision and its evidence must
// remain byte-identical afterwards.
func TestLateDeliveryPreservesHistoricalConclusion(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	base := sineClip(48000, 4800, 1, 440, 0.4)
	baselinePCM := pcmFromSamples(48000, 1, base, QuantRoundHalfAway)
	baselineWAV := mustWAVFromPCM(t, baselinePCM)

	// A legal numeric difference: sub-LSB dither.
	lateSignal := make([]float64, len(base))
	copy(lateSignal, base)
	for index := range lateSignal {
		value := math.Sin(float64(index)*12.9898) * 43758.5453
		fraction := value - math.Floor(value)
		lateSignal[index] += 0.6 * (2*fraction - 1) / 32767
	}
	latePCM := pcmFromSamples(48000, 1, lateSignal, QuantRoundHalfAway)
	lateWAV := mustWAVFromPCM(t, latePCM)

	source := baselinePCM
	baselineSourceDigest := DigestBytes(mustWAVFromPCM(t, source))

	baselineCandidate := Candidate{
		ID:               "historical_render",
		Role:             "historical_delivery",
		CreatedAt:        "2026-01-15T09:00:00Z",
		ProcessorVersion: "production-v1",
		TimelineID:       "timeline_01",
		SampleRate:       48000,
		OutputChannels:   1,
		Post:             PostProcessing{NormalizationTargetDBFS: -1, AppliedGainDB: 0, Quantization: QuantRoundHalfAway},
		Spans: []CandidateSpan{{
			Selection: Selection{
				SourceID: "src", SourceDigest: baselineSourceDigest,
				SourceStartFrame: 0, SourceEndFrame: 4800,
				SourceSampleRate: 48000, SourceChannels: 1,
			},
			Processing: SpanProcessing{GainDB: 0, ResampleMethod: ResampleLinear, ChannelMapping: MappingUnchanged},
		}},
	}
	targets := baseTargets(DigestBytes(baselineWAV))
	targets.TargetPeakDBFS = mustObservation(t, baselinePCM).PeakDBFS
	if _, err := store.Create(context.Background(), NewRecord{
		RecordID: "record_late",
		Title:    "late delivery rehearsal",
		Actor:    "historical-producer",
		Targets:  targets,
		Baseline: baselineCandidate,
		Artifacts: []Artifact{
			{Kind: EvidenceDownloadedPCM, Subject: "output", Extension: "wav", Bytes: baselineWAV},
			{Kind: EvidenceSourceWAV, Subject: "src", Extension: "wav", Bytes: mustWAVFromPCM(t, source)},
			{Kind: EvidenceReceipt, Subject: "receipt", Extension: "json", Bytes: receiptJSON(t, baselineCandidate, baselinePCM, DigestBytes(baselineWAV))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordDecision(context.Background(), "record_late", DecisionInput{
		Actor: "reviewer", CandidateID: "historical_render",
		Rationale: "accepted historical delivery", Historical: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Snapshot the historical files before the late candidate arrives.
	recordDir, err := store.recordDir("record_late")
	if err != nil {
		t.Fatal(err)
	}
	baselineCandidatePath := filepath.Join(recordDir, "candidates", "historical_render", "candidate.json")
	before, err := os.ReadFile(baselineCandidatePath)
	if err != nil {
		t.Fatal(err)
	}
	decisionBefore, err := os.ReadFile(filepath.Join(recordDir, "decisions", "0001-historical_render.json"))
	if err != nil {
		t.Fatal(err)
	}

	// The late candidate is produced before the audit but submitted after the
	// historical acceptance.
	lateCandidate := Candidate{
		ID:               "third_party_render",
		Role:             "new_render",
		CreatedAt:        "2026-01-10T12:00:00Z",
		ProcessorVersion: "third-party-v2",
		TimelineID:       "timeline_01",
		SampleRate:       48000,
		OutputChannels:   1,
		Post:             PostProcessing{NormalizationTargetDBFS: -1, AppliedGainDB: 0, Quantization: QuantRoundHalfAway},
		Spans:            baselineCandidate.Spans,
	}
	if _, _, err := store.Ingest(context.Background(), "record_late", IngestInput{
		Candidate: lateCandidate,
		Artifacts: []Artifact{
			{Kind: EvidenceDownloadedPCM, Subject: "output", Extension: "wav", Bytes: lateWAV},
			{Kind: EvidenceSourceWAV, Subject: "src", Extension: "wav", Bytes: mustWAVFromPCM(t, source)},
			{Kind: EvidenceReceipt, Subject: "receipt", Extension: "json", Bytes: receiptJSON(t, lateCandidate, latePCM, DigestBytes(lateWAV))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	verdict, err := store.Review(context.Background(), "record_late", "third_party_render")
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Outcome != OutcomeCompatibleAccept {
		t.Fatalf("a dither-only late delivery produced %q, want %q", verdict.Outcome, OutcomeCompatibleAccept)
	}
	if _, err := store.RecordDecision(context.Background(), "record_late", DecisionInput{
		Actor: "reviewer", CandidateID: "third_party_render",
		Rationale: "dither-only requantization accepted as compatible",
	}); err != nil {
		t.Fatal(err)
	}

	// The historical candidate file must be byte-identical.
	after, err := os.ReadFile(baselineCandidatePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the historical candidate description was rewritten by the late delivery")
	}
	decisionAfter, err := os.ReadFile(filepath.Join(recordDir, "decisions", "0001-historical_render.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(decisionBefore) != string(decisionAfter) {
		t.Fatal("the historical decision was rewritten by the later decision")
	}
	decisions, err := store.LoadDecisions(context.Background(), "record_late")
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 {
		t.Fatalf("expected the late decision to be appended, found %d decisions", len(decisions))
	}
	if decisions[0].Sequence != 1 || decisions[0].CandidateID != "historical_render" || !decisions[0].Historical {
		t.Fatal("the first decision was not preserved at sequence 1")
	}
	if decisions[1].Sequence != 2 || decisions[1].CandidateID != "third_party_render" {
		t.Fatal("the late decision was not appended at sequence 2")
	}
	// And the historical bytes themselves are still intact.
	if err := store.RecheckCandidate(context.Background(), "record_late", "historical_render"); err != nil {
		t.Fatalf("historical evidence did not survive the late delivery: %v", err)
	}
	if _, err := store.VerifyChain(context.Background(), "record_late"); err != nil {
		t.Fatalf("event chain did not verify: %v", err)
	}
}

// TestReviewIsNotRecomputed asserts that a recorded verdict is returned
// verbatim instead of being recomputed under the same rule set.
func TestReviewIsNotRecomputed(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	base := sineClip(48000, 2400, 1, 440, 0.4)
	baselinePCM := pcmFromSamples(48000, 1, base, QuantRoundHalfAway)
	baselineWAV := mustWAVFromPCM(t, baselinePCM)
	changed := make([]float64, len(base))
	copy(changed, base)
	for index := range changed {
		changed[index] += 0.0015 * math.Sin(float64(index)*1.6180339887)
	}
	changedWAV := mustWAVFromPCM(t, pcmFromSamples(48000, 1, changed, QuantRoundHalfAway))

	targets := baseTargets(DigestBytes(baselineWAV))
	spans := []CandidateSpan{{
		Selection: Selection{
			SourceID: "src", SourceDigest: DigestBytes(mustWAVFromPCM(t, baselinePCM)),
			SourceStartFrame: 0, SourceEndFrame: 2400, SourceSampleRate: 48000, SourceChannels: 1,
		},
		Processing: SpanProcessing{GainDB: 0, ResampleMethod: ResampleLinear, ChannelMapping: MappingUnchanged},
	}}
	baseline := Candidate{ID: "baseline_01", Role: "historical_delivery", SampleRate: 48000, OutputChannels: 1,
		Post: PostProcessing{NormalizationTargetDBFS: -1, Quantization: QuantRoundHalfAway}, Spans: spans}
	if _, err := store.Create(context.Background(), NewRecord{
		RecordID: "record_rerun", Title: "re-review", Actor: "producer", Targets: targets, Baseline: baseline,
		Artifacts: []Artifact{
			{Kind: EvidenceDownloadedPCM, Subject: "output", Extension: "wav", Bytes: baselineWAV},
			{Kind: EvidenceSourceWAV, Subject: "src", Extension: "wav", Bytes: mustWAVFromPCM(t, baselinePCM)},
			{Kind: EvidenceReceipt, Subject: "receipt", Extension: "json", Bytes: receiptJSON(t, baseline, baselinePCM, DigestBytes(baselineWAV))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{ID: "candidate_01", Role: "new_render", SampleRate: 48000, OutputChannels: 1,
		Post: PostProcessing{NormalizationTargetDBFS: -1, Quantization: QuantRoundHalfAway}, Spans: spans}
	if _, _, err := store.Ingest(context.Background(), "record_rerun", IngestInput{
		Candidate: candidate,
		Artifacts: []Artifact{
			{Kind: EvidenceDownloadedPCM, Subject: "output", Extension: "wav", Bytes: changedWAV},
			{Kind: EvidenceSourceWAV, Subject: "src", Extension: "wav", Bytes: mustWAVFromPCM(t, baselinePCM)},
			{Kind: EvidenceReceipt, Subject: "receipt", Extension: "json", Bytes: receiptJSON(t, candidate, pcmFromSamples(48000, 1, changed, QuantRoundHalfAway), DigestBytes(changedWAV))},
		},
	}); err != nil {
		t.Fatal(err)
	}
	first, err := store.Review(context.Background(), "record_rerun", "candidate_01")
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != OutcomeBlockRelease {
		t.Fatalf("a real waveform change produced %q, want block", first.Outcome)
	}
	second, err := store.Review(context.Background(), "record_rerun", "candidate_01")
	if err != nil {
		t.Fatal(err)
	}
	if second.DecidedAt != first.DecidedAt || second.Outcome != first.Outcome {
		t.Fatal("re-review recomputed a recorded verdict instead of returning the original")
	}
}
