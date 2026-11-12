package audit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IngestInput is one candidate submission plus the evidence the producer
// supplied. Artifacts must include the downloaded PCM and, when the producer
// has one, the receipt.
type IngestInput struct {
	Candidate Candidate
	Artifacts []Artifact
}

// Ingest registers a candidate, verifies each evidence digest on write,
// decodes the downloaded PCM independently, and records the observation.
// It never touches a previously registered candidate.
func (store *Store) Ingest(ctx context.Context, recordID string, input IngestInput) (Candidate, Observation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := readContextErr(ctx); err != nil {
		return Candidate{}, Observation{}, err
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return Candidate{}, Observation{}, err
	}
	if _, statErr := os.Stat(store.recordPath(recordDir)); statErr != nil {
		return Candidate{}, Observation{}, fmt.Errorf("audit record %q does not exist", recordID)
	}
	candidate, err := store.addCandidateLocked(ctx, recordDir, input.Candidate, input.Artifacts)
	if err != nil {
		return Candidate{}, Observation{}, err
	}
	observation, err := store.observeCandidateLocked(ctx, recordID, candidate)
	if err != nil {
		// A candidate whose downloaded PCM is missing or undecodable is a
		// bad submission, not a half-written record. Remove it so the caller
		// can correct and resubmit without a duplicate-identifier error, and
		// keep the event log unchanged because the candidate never existed
		// from the reviewer's point of view.
		_ = os.RemoveAll(filepath.Join(recordDir, "candidates", candidate.ID))
		return Candidate{}, Observation{}, err
	}
	return candidate, observation, nil
}

func (store *Store) observeCandidate(ctx context.Context, recordID string, candidate Candidate) (Observation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.observeCandidateLocked(ctx, recordID, candidate)
}

func (store *Store) observeCandidateLocked(ctx context.Context, recordID string, candidate Candidate) (Observation, error) {
	evidence, ok := ListedEvidence(candidate, EvidenceDownloadedPCM, "")
	if !ok {
		return Observation{}, errors.New("candidate supplied no downloaded PCM evidence")
	}
	payload, err := store.ReadEvidence(ctx, recordID, candidate.ID, evidence)
	if err != nil {
		return Observation{}, err
	}
	pcm, err := DecodePCM16Wave(payload)
	if err != nil {
		return Observation{}, fmt.Errorf("downloaded PCM does not decode: %w", err)
	}
	observation, err := ObservePCM(ctx, candidate.ID, evidence.SHA256, pcm, store.clock())
	if err != nil {
		return Observation{}, err
	}
	if err := store.saveObservationLocked(ctx, recordID, candidate.ID, observation); err != nil {
		return Observation{}, err
	}
	return observation, nil
}

// ReviewInput names the candidate to review. The baseline comes from the
// record itself and its stored evidence; nothing is accepted from the caller.
type ReviewInput struct {
	CandidateID string
}

// Review performs the full independent review of a candidate against the
// record's baseline and appends the reconstruction and verdict.
//
// The sequence is fixed:
//
//  1. Load the baseline and the candidate from the store.
//  2. Re-verify every evidence digest.
//  3. Rebuild the candidate from its declared selection and conditions.
//  4. Rebuild the baseline from its declared selection and conditions.
//  5. Compare structures and waveforms, then evaluate under the rule set.
func (store *Store) Review(ctx context.Context, recordID, candidateID string) (Verdict, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := readContextErr(ctx); err != nil {
		return Verdict{}, err
	}
	record, err := store.LoadRecord(ctx, recordID)
	if err != nil {
		return Verdict{}, err
	}
	if candidateID == "" || candidateID == record.BaselineCandidate {
		return Verdict{}, errors.New("a review needs a non-baseline candidate identifier")
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return Verdict{}, err
	}
	// A conclusion recorded under this rule set is not recomputed. The
	// original verdict is a published decision; a later review appends new
	// evidence or a new rule set instead of quietly changing the old answer.
	if existing, found, err := store.existingVerdict(recordDir, candidateID, record.RuleSetID); err != nil {
		return Verdict{}, err
	} else if found {
		return existing, nil
	}

	candidate, err := store.LoadCandidate(ctx, recordID, candidateID)
	if err != nil {
		return Verdict{}, err
	}
	baseline, err := store.LoadCandidate(ctx, recordID, record.BaselineCandidate)
	if err != nil {
		return Verdict{}, err
	}

	// All evidence for both candidates is verified before any comparison, so
	// a tampered artifact cannot influence a verdict.
	if err := store.verifyCandidateEvidence(ctx, recordID, candidate); err != nil {
		return Verdict{}, err
	}
	if err := store.verifyCandidateEvidence(ctx, recordID, baseline); err != nil {
		return Verdict{}, err
	}

	downloadedBytes, err := store.evidenceBytes(ctx, recordID, candidate, EvidenceDownloadedPCM)
	if err != nil {
		return Verdict{}, err
	}
	baselineBytes, err := store.evidenceBytes(ctx, recordID, baseline, EvidenceDownloadedPCM)
	if err != nil {
		return Verdict{}, err
	}
	downloaded, err := DecodePCM16Wave(downloadedBytes)
	if err != nil {
		return Verdict{}, fmt.Errorf("candidate PCM does not decode: %w", err)
	}
	baselinePCM, err := DecodePCM16Wave(baselineBytes)
	if err != nil {
		return Verdict{}, fmt.Errorf("baseline PCM does not decode: %w", err)
	}
	downloadedDigest := digestBytes(downloadedBytes)
	baselineDigest := digestBytes(baselineBytes)

	sources, err := loadRecordSources(ctx, store, recordID, record)
	if err != nil {
		return Verdict{}, err
	}

	candidateObservation, err := store.LoadObservation(ctx, recordID, candidateID)
	if err != nil {
		return Verdict{}, err
	}
	baselineObservation, err := store.LoadObservation(ctx, recordID, record.BaselineCandidate)
	if err != nil {
		return Verdict{}, err
	}

	receiptEvidence, err := loadReceipt(ctx, store, recordID, candidate)
	if err != nil {
		return Verdict{}, err
	}

	reconstruction, reconFindings, rebuildErr := Rebuild(ctx, RebuildInput{
		Candidate:        &candidate,
		Sources:          sources,
		Downloaded:       downloaded,
		DownloadedDigest: downloadedDigest,
		Receipt:          receiptEvidence,
		Targets:          record.FidelityTargets,
		RuleSetID:        record.RuleSetID,
		BuiltAt:          store.clock(),
	})
	if rebuildErr != nil {
		reconstruction.Available = false
		reconstruction.Reason = rebuildErr.Error()
		reconstruction.CandidateID = candidateID
		reconstruction.RuleSetID = record.RuleSetID
		reconstruction.BuiltAt = store.clock()
		reconFindings = []Finding{{
			Class:      "reconstruction_not_possible",
			Attributes: []string{"insufficient"},
			Detail:     rebuildErr.Error(),
			Blocking:   true,
		}}
	} else {
		reconstruction.Available = true
	}
	if err := store.saveReconstructionLocked(ctx, recordID, candidateID, reconstruction); err != nil {
		return Verdict{}, err
	}

	structure := CompareStructure(baselinePCM, downloaded)
	waveform, err := CompareWaveforms(ctx, baselinePCM, downloaded, record.FidelityTargets.QuantizationToleranceLSB)
	if err != nil {
		return Verdict{}, err
	}

	byteIdentical := digestBytes(downloadedBytes) == baselineDigest
	verdict, err := Evaluate(ctx, EvaluateInput{
		Candidate:      &candidate,
		Baseline:       &baseline,
		Targets:        record.FidelityTargets,
		Observation:    &candidateObservation,
		BaselineObs:    &baselineObservation,
		Waveform:       waveform,
		Structure:      structure,
		Reconstruction: &reconstruction,
		ReconFindings:  reconFindings,
		ByteIdentical:  byteIdentical,
		RuleSetID:      record.RuleSetID,
		DecidedAt:      store.clock(),
	})
	if err != nil {
		return Verdict{}, err
	}
	verdict.DownloadedDigest = downloadedDigest
	verdict.DownloadedFrames = downloaded.FrameCount
	verdict.ReconstructionAvailable = reconstruction.Available
	verdict.ReconstructionMatches = reconstruction.MatchesDownloaded
	verdict.ReconstructionFrames = reconstruction.ExpectedFrames
	if verdict.Outcome == OutcomeReleasePass && record.FidelityTargets.HistoricalOutputDigest != "" &&
		downloadedDigest != record.FidelityTargets.HistoricalOutputDigest {
		// A byte-identical pass must still honor the historical promise when
		// one is declared and differs; it cannot be a pass on identity alone.
		verdict.Outcome = OutcomeCompatibleAccept
		verdict.ByteIdenticalToBaseline = false
		verdict.Reasons = append(verdict.Reasons,
			"download matches the baseline candidate but not the declared historical delivery digest")
	}
	if err := store.saveVerdictLocked(ctx, recordID, verdict); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

func loadRecordSources(ctx context.Context, store *Store, recordID string, record ChangeRecord) (map[string]*PCM, error) {
	// Sources are declared per candidate. They are shared by digest, so a set
	// of source digests is loaded once and indexed by both ID and digest.
	sources := make(map[string]*PCM)
	ids, err := store.ListedCandidates(ctx, recordID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		candidate, loadErr := store.LoadCandidate(ctx, recordID, id)
		if loadErr != nil {
			return nil, loadErr
		}
		for _, evidence := range candidate.Evidence {
			if evidence.Kind != EvidenceSourceWAV {
				continue
			}
			if _, exists := sources[evidence.SHA256]; exists {
				continue
			}
			payload, readErr := store.ReadEvidence(ctx, recordID, id, evidence)
			if readErr != nil {
				return nil, readErr
			}
			pcm, decodeErr := DecodePCM16Wave(payload)
			if decodeErr != nil {
				return nil, fmt.Errorf("source evidence %s does not decode: %w", evidence.SHA256, decodeErr)
			}
			sources[evidence.SHA256] = pcm
		}
	}
	// Index by declared source ID as well.
	for _, id := range ids {
		candidate, loadErr := store.LoadCandidate(ctx, recordID, id)
		if loadErr != nil {
			return nil, loadErr
		}
		for _, span := range candidate.Spans {
			if _, exists := sources[span.Selection.SourceID]; exists {
				continue
			}
			if pcm, exists := sources[span.Selection.SourceDigest]; exists {
				sources[span.Selection.SourceID] = pcm
			}
		}
	}
	return sources, nil
}

func (store *Store) verifyCandidateEvidence(ctx context.Context, recordID string, candidate Candidate) error {
	for _, evidence := range candidate.Evidence {
		if _, err := store.ReadEvidence(ctx, recordID, candidate.ID, evidence); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) evidenceBytes(ctx context.Context, recordID string, candidate Candidate, kind string) ([]byte, error) {
	evidence, ok := ListedEvidence(candidate, kind, "")
	if !ok {
		return nil, fmt.Errorf("candidate %q has no %s evidence", candidate.ID, kind)
	}
	return store.ReadEvidence(ctx, recordID, candidate.ID, evidence)
}

func loadReceipt(ctx context.Context, store *Store, recordID string, candidate Candidate) (*ReceiptEvidence, error) {
	evidence, ok := ListedEvidence(candidate, EvidenceReceipt, "")
	if !ok {
		return &ReceiptEvidence{Present: false}, nil
	}
	payload, err := store.ReadEvidence(ctx, recordID, candidate.ID, evidence)
	if err != nil {
		return nil, err
	}
	receipt, err := ParseReceiptEvidence(payload)
	if err != nil {
		return nil, err
	}
	return &receipt, nil
}

// ReportPath is the deterministic location of a candidate's human-readable
// report, keyed by the verdict rule set so an older report is never replaced.
func ReportPath(recordDir, candidateID, ruleSetID string) string {
	name := sanitizeComponent(candidateID) + "." + sanitizeComponent(strings.ReplaceAll(ruleSetID, "/", "-")) + ".md"
	return filepath.Join(recordDir, "reports", name)
}

// WriteReport renders and stores a markdown report for a verdict. A report is
// keyed by candidate and rule set, so a later rule set appends rather than
// overwrites.
func (store *Store) WriteReport(ctx context.Context, recordID, candidateID string, verdict Verdict) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := readContextErr(ctx); err != nil {
		return "", err
	}
	record, err := store.LoadRecord(ctx, recordID)
	if err != nil {
		return "", err
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return "", err
	}
	markdown := RenderReport(record, verdict)
	path := ReportPath(recordDir, candidateID, verdict.RuleSetID)
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if string(existing) != markdown {
			return "", fmt.Errorf("%w: %s", ErrImmutableResult, filepath.Base(path))
		}
		return path, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	if err := writeExclusive(path, []byte(markdown), 0o640); err != nil {
		return "", err
	}
	if err := store.appendEvent(recordDir, "reviewer", "report_rendered", candidateID,
		fmt.Sprintf("rule_set=%s outcome=%s", verdict.RuleSetID, verdict.Outcome),
		digestBytes([]byte(markdown))); err != nil {
		return "", err
	}
	return path, nil
}

// EnsureRecordDirectory is used by the demonstration command to find the
// record root.
func (store *Store) EnsureRecordDirectory(recordID string) (string, error) {
	return store.recordDir(recordID)
}
