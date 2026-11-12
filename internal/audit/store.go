package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var recordIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{2,63}$`)

// Store is an append-only audit repository rooted at one directory. Every
// mutation creates a new file or appends a hash-chained event; no call ever
// rewrites a candidate, observation, reconstruction, verdict, or earlier
// decision.
type Store struct {
	root string
	mu   sync.Mutex
	now  func() time.Time
}

// NewStore opens (or creates) a store rooted at root.
func NewStore(root string) (*Store, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return nil, err
	}
	return &Store{root: absolute, now: func() time.Time { return time.Now().UTC() }}, nil
}

// SetClock overrides the clock; it exists for deterministic demonstrations.
func (store *Store) SetClock(now func() time.Time) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.now = now
}

func (store *Store) clock() string {
	return store.now().UTC().Format(time.RFC3339Nano)
}

func (store *Store) recordDir(recordID string) (string, error) {
	if !recordIDPattern.MatchString(recordID) {
		return "", fmt.Errorf("record identifier %q is not a safe directory name", recordID)
	}
	return filepath.Join(store.root, recordID), nil
}

func (store *Store) candidateDir(recordDir, candidateID string) (string, error) {
	if !recordIDPattern.MatchString(candidateID) {
		return "", fmt.Errorf("candidate identifier %q is not a safe directory name", candidateID)
	}
	return filepath.Join(recordDir, "candidates", candidateID), nil
}

func (store *Store) recordPath(recordDir string) string {
	return filepath.Join(recordDir, "record.json")
}

func (store *Store) eventsPath(recordDir string) string {
	return filepath.Join(recordDir, "events.jsonl")
}

func (store *Store) decisionsDir(recordDir string) string {
	return filepath.Join(recordDir, "decisions")
}

func (store *Store) verdictsDir(recordDir string) string {
	return filepath.Join(recordDir, "verdicts")
}

func (store *Store) reportsDir(recordDir string) string {
	return filepath.Join(recordDir, "reports")
}

// NewRecord is the one-time creation input. Fidelity targets are captured
// here and are never recomputed afterwards.
type NewRecord struct {
	RecordID  string
	Title     string
	Actor     string
	Targets   FidelityTargets
	Baseline  Candidate
	Artifacts []Artifact
}

// Create writes a record and its baseline candidate. It refuses to overwrite
// an existing record identifier; a re-run must use a new record or the
// append paths.
func (store *Store) Create(ctx context.Context, input NewRecord) (ChangeRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := readContextErr(ctx); err != nil {
		return ChangeRecord{}, err
	}
	if err := validateTargets(input.Targets); err != nil {
		return ChangeRecord{}, err
	}
	recordDir, err := store.recordDir(input.RecordID)
	if err != nil {
		return ChangeRecord{}, err
	}
	if _, statErr := os.Stat(recordDir); statErr == nil {
		return ChangeRecord{}, fmt.Errorf("audit record %q already exists; append to it instead of recreating", input.RecordID)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return ChangeRecord{}, statErr
	}
	applyDeclaredDefaults(&input.Baseline)
	if input.Baseline.ID == "" {
		return ChangeRecord{}, errors.New("baseline candidate needs an identifier")
	}
	if err := ensureDir(recordDir); err != nil {
		return ChangeRecord{}, err
	}

	record := ChangeRecord{
		SchemaVersion:     RecordSchemaVersion,
		RecordID:          input.RecordID,
		Title:             input.Title,
		CreatedAt:         store.clock(),
		FidelityTargets:   input.Targets,
		RuleSetID:         RuleSetID,
		BaselineCandidate: input.Baseline.ID,
		Actor:             input.Actor,
	}
	payload, err := marshalIndented(record)
	if err != nil {
		return ChangeRecord{}, err
	}
	if err := writeExclusive(store.recordPath(recordDir), payload, 0o640); err != nil {
		return ChangeRecord{}, err
	}
	if err := store.appendEvent(recordDir, input.Actor, "record_created", input.RecordID,
		"fidelity targets captured: "+describeTargets(input.Targets), digestBytes(payload)); err != nil {
		return ChangeRecord{}, err
	}
	if _, err := store.addCandidateLocked(ctx, recordDir, input.Baseline, input.Artifacts); err != nil {
		return ChangeRecord{}, err
	}
	baseline, err := store.LoadCandidate(ctx, input.RecordID, input.Baseline.ID)
	if err != nil {
		return ChangeRecord{}, err
	}
	if _, ok := ListedEvidence(baseline, EvidenceDownloadedPCM, ""); ok {
		if _, err := store.observeCandidateLocked(ctx, input.RecordID, baseline); err != nil {
			return ChangeRecord{}, err
		}
	}
	return record, nil
}

func describeTargets(targets FidelityTargets) string {
	parts := []string{
		fmt.Sprintf("%d Hz", targets.SampleRate),
		fmt.Sprintf("%d ch", targets.Channels),
		fmt.Sprintf("%d-bit", targets.BitsPerSample),
		fmt.Sprintf("peak %.2f +/- %.2f dBFS", targets.TargetPeakDBFS, targets.PeakToleranceDB),
		fmt.Sprintf("loudness +/- %.2f dB", targets.LoudnessToleranceDB),
		fmt.Sprintf("quantization <= %.2f LSB", targets.QuantizationToleranceLSB),
	}
	if targets.HistoricalOutputDigest != "" {
		parts = append(parts, "historical digest "+targets.HistoricalOutputDigest)
	}
	return strings.Join(parts, ", ")
}

func validateTargets(targets FidelityTargets) error {
	if targets.SampleRate < 8000 || targets.SampleRate > 192000 {
		return errors.New("fidelity target sample rate is outside the supported range")
	}
	if targets.Channels != 1 && targets.Channels != 2 {
		return errors.New("fidelity target channels must be one or two")
	}
	if targets.BitsPerSample != 16 {
		return errors.New("fidelity target bit width must be 16")
	}
	if !finite(targets.TargetPeakDBFS) || targets.TargetPeakDBFS >= 0 || targets.TargetPeakDBFS < -60 {
		return errors.New("fidelity target peak must be between -60 and 0 dBFS")
	}
	if !finite(targets.PeakToleranceDB) || targets.PeakToleranceDB < 0 || targets.PeakToleranceDB > 3 {
		return errors.New("peak tolerance must be between 0 and 3 dB")
	}
	if !finite(targets.LoudnessToleranceDB) || targets.LoudnessToleranceDB < 0 || targets.LoudnessToleranceDB > 12 {
		return errors.New("loudness tolerance must be between 0 and 12 dB")
	}
	if targets.QuantizationToleranceLSB < 0 || targets.QuantizationToleranceLSB > 8 {
		return errors.New("quantization tolerance must be between 0 and 8 LSB")
	}
	if targets.DurationToleranceMS < 0 || targets.DurationToleranceMS > 50 {
		return errors.New("duration tolerance must be between 0 and 50 ms")
	}
	return nil
}

// LoadRecord reads the immutable record header.
func (store *Store) LoadRecord(ctx context.Context, recordID string) (ChangeRecord, error) {
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return ChangeRecord{}, err
	}
	payload, err := os.ReadFile(store.recordPath(recordDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ChangeRecord{}, fmt.Errorf("audit record %q does not exist", recordID)
		}
		return ChangeRecord{}, err
	}
	var record ChangeRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return ChangeRecord{}, fmt.Errorf("decode record: %w", err)
	}
	return record, nil
}

// AddCandidate appends a candidate. Submitting an identifier twice is an
// error, never an overwrite, even when the content is identical: the arrival
// of a later candidate must not erase the earlier one.
func (store *Store) AddCandidate(ctx context.Context, recordID string, candidate Candidate, artifacts []Artifact) (Candidate, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := readContextErr(ctx); err != nil {
		return Candidate{}, err
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return Candidate{}, err
	}
	if _, err := os.Stat(store.recordPath(recordDir)); err != nil {
		return Candidate{}, fmt.Errorf("audit record %q does not exist", recordID)
	}
	applyDeclaredDefaults(&candidate)
	return store.addCandidateLocked(ctx, recordDir, candidate, artifacts)
}

func (store *Store) addCandidateLocked(ctx context.Context, recordDir string, candidate Candidate, artifacts []Artifact) (Candidate, error) {
	if !recordIDPattern.MatchString(candidate.ID) {
		return Candidate{}, fmt.Errorf("candidate identifier %q is not a safe directory name", candidate.ID)
	}
	candidateDir, err := store.candidateDir(recordDir, candidate.ID)
	if err != nil {
		return Candidate{}, err
	}
	if err := ensureDir(candidateDir); err != nil {
		return Candidate{}, err
	}
	candidatePath := filepath.Join(candidateDir, "candidate.json")
	if _, statErr := os.Stat(candidatePath); statErr == nil {
		return Candidate{}, fmt.Errorf("%w: %s", ErrDuplicateCandidate, candidate.ID)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Candidate{}, statErr
	}

	evidence := make([]Evidence, 0, len(artifacts))
	for _, artifact := range artifacts {
		stored, storeErr := store.writeEvidence(candidateDir, artifact)
		if storeErr != nil {
			return Candidate{}, storeErr
		}
		evidence = append(evidence, stored)
	}
	candidate.Evidence = evidence
	if candidate.CreatedAt == "" {
		candidate.CreatedAt = store.clock()
	}
	payload, err := marshalIndented(candidate)
	if err != nil {
		return Candidate{}, err
	}
	if err := writeExclusive(candidatePath, payload, 0o640); err != nil {
		return Candidate{}, err
	}
	if err := store.appendEvent(recordDir, candidateRoleActor(candidate), "candidate_added", candidate.ID,
		fmt.Sprintf("role=%s processor=%s created_at=%s evidence=%d", candidate.Role, candidate.ProcessorVersion, candidate.CreatedAt, len(evidence)),
		digestBytes(payload)); err != nil {
		return Candidate{}, err
	}
	return candidate, nil
}

func candidateRoleActor(candidate Candidate) string {
	if strings.TrimSpace(candidate.ProcessorVersion) != "" {
		return "processor:" + candidate.ProcessorVersion
	}
	return "producer"
}

// writeEvidence stores artifact bytes content-addressed under the candidate
// folder. Existing identical bytes are reused; existing different bytes at
// the same digest are impossible by construction, and a digest collision is
// detected rather than overwritten.
func (store *Store) writeEvidence(candidateDir string, artifact Artifact) (Evidence, error) {
	kind := strings.TrimSpace(artifact.Kind)
	if kind == "" {
		return Evidence{}, errors.New("evidence kind is required")
	}
	if len(artifact.Bytes) == 0 {
		return Evidence{}, fmt.Errorf("evidence %q is empty", kind)
	}
	digest := digestBytes(artifact.Bytes)
	subject := sanitizeComponent(artifact.Subject)
	if subject == "" {
		subject = "_"
	}
	directory := filepath.Join(candidateDir, "evidence", sanitizeComponent(kind), subject)
	if err := ensureDir(directory); err != nil {
		return Evidence{}, err
	}
	extension := sanitizeExtension(artifact.Extension)
	name := digest
	if extension != "" {
		name += "." + extension
	}
	relative, err := filepath.Rel(candidateDir, filepath.Join(directory, name))
	if err != nil {
		return Evidence{}, err
	}
	path := filepath.Join(directory, name)
	if _, statErr := os.Stat(path); statErr == nil {
		stored, _, verifyErr := digestFile(path)
		if verifyErr != nil {
			return Evidence{}, verifyErr
		}
		if stored != digest {
			return Evidence{}, fmt.Errorf("%w: %s", ErrEvidenceDrift, path)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Evidence{}, statErr
	} else if err := writeExclusive(path, artifact.Bytes, 0o640); err != nil {
		return Evidence{}, err
	}
	return Evidence{
		Kind:         kind,
		Subject:      strings.TrimSpace(artifact.Subject),
		RelativePath: relative,
		SHA256:       digest,
		ByteCount:    int64(len(artifact.Bytes)),
		MediaType:    strings.TrimSpace(artifact.MediaType),
		Description:  strings.TrimSpace(artifact.Description),
	}, nil
}

func sanitizeComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var builder strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_', character == '.':
			builder.WriteRune(character)
		default:
			builder.WriteRune('_')
		}
	}
	return strings.Trim(builder.String(), "._")
}

func sanitizeExtension(value string) string {
	value = sanitizeComponent(value)
	if len(value) > 8 {
		value = value[:8]
	}
	return value
}

// ReadEvidence returns verified evidence bytes and re-checks the digest, so a
// reviewer notices tampering instead of trusting a stale locator.
func (store *Store) ReadEvidence(ctx context.Context, recordID, candidateID string, evidence Evidence) ([]byte, error) {
	if err := readContextErr(ctx); err != nil {
		return nil, err
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return nil, err
	}
	candidateDir, err := store.candidateDir(recordDir, candidateID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(candidateDir, evidence.RelativePath)
	// RelativePath is stored in the record, so it is not trusted blindly: a
	// path that escapes the candidate folder must not be readable.
	if relative, relErr := filepath.Rel(candidateDir, path); relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("evidence locator %q escapes the candidate folder", evidence.RelativePath)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	digest, count, err := digestFile(path)
	if err != nil {
		return nil, err
	}
	if digest != evidence.SHA256 || count != evidence.ByteCount {
		return nil, fmt.Errorf("%w: %s", ErrEvidenceDrift, evidence.RelativePath)
	}
	return payload, nil
}

// LoadCandidate reads the immutable candidate description.
func (store *Store) LoadCandidate(ctx context.Context, recordID, candidateID string) (Candidate, error) {
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return Candidate{}, err
	}
	candidateDir, err := store.candidateDir(recordDir, candidateID)
	if err != nil {
		return Candidate{}, err
	}
	payload, err := os.ReadFile(filepath.Join(candidateDir, "candidate.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Candidate{}, fmt.Errorf("candidate %q does not exist in record %q", candidateID, recordID)
		}
		return Candidate{}, err
	}
	var candidate Candidate
	if err := json.Unmarshal(payload, &candidate); err != nil {
		return Candidate{}, fmt.Errorf("decode candidate %q: %w", candidateID, err)
	}
	return candidate, nil
}

// ListedEvidence returns the evidence entry for a kind/subject pair.
func ListedEvidence(candidate Candidate, kind, subject string) (Evidence, bool) {
	for _, evidence := range candidate.Evidence {
		if evidence.Kind == kind && (subject == "" || evidence.Subject == subject) {
			return evidence, true
		}
	}
	return Evidence{}, false
}

func (store *Store) appendEvent(recordDir, actor, action, subject, detail, payloadDigest string) error {
	if err := ensureDir(recordDir); err != nil {
		return err
	}
	events, err := readEvents(store.eventsPath(recordDir))
	if err != nil {
		return err
	}
	previous := ""
	sequence := 1
	if len(events) > 0 {
		last := events[len(events)-1]
		previous = last.Hash
		sequence = last.Sequence + 1
	}
	occurredAt := store.clock()
	event := AuditEvent{
		Sequence:      sequence,
		OccurredAt:    occurredAt,
		Actor:         actor,
		Action:        action,
		Subject:       subject,
		Detail:        detail,
		PayloadDigest: payloadDigest,
		PreviousHash:  previous,
	}
	event.Hash = eventHash(sequence, occurredAt, actor, action, subject, detail, payloadDigest, previous)
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	handle, err := os.OpenFile(store.eventsPath(recordDir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := handle.Write(append(payload, '\n')); err != nil {
		_ = handle.Close()
		return err
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return err
	}
	return handle.Close()
}

func readEvents(path string) ([]AuditEvent, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(payload), "\n"), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return nil, nil
	}
	events := make([]AuditEvent, 0, len(lines))
	for _, line := range lines {
		var event AuditEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("decode audit event: %w", err)
		}
		events = append(events, event)
	}
	return events, nil
}

// VerifyChain recomputes the event hash chain. A tampered or reordered log
// fails verification, which is how a later reviewer proves that earlier
// evidence was not rewritten.
func (store *Store) VerifyChain(ctx context.Context, recordID string) ([]AuditEvent, error) {
	if err := readContextErr(ctx); err != nil {
		return nil, err
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return nil, err
	}
	events, err := readEvents(store.eventsPath(recordDir))
	if err != nil {
		return nil, err
	}
	previous := ""
	for index, event := range events {
		if event.Sequence != index+1 {
			return nil, fmt.Errorf("audit event %d has sequence %d", index, event.Sequence)
		}
		if event.PreviousHash != previous {
			return nil, fmt.Errorf("audit event %d previous hash does not link to event %d", event.Sequence, index)
		}
		expected := eventHash(event.Sequence, event.OccurredAt, event.Actor, event.Action,
			event.Subject, event.Detail, event.PayloadDigest, event.PreviousHash)
		if expected != event.Hash {
			return nil, fmt.Errorf("audit event %d hash does not verify", event.Sequence)
		}
		previous = event.Hash
	}
	return events, nil
}

// writeOnce writes a per-candidate result file. Existing identical content is
// idempotent; existing different content is refused.
func writeOnce(path string, value any) error {
	payload, err := marshalIndented(value)
	if err != nil {
		return err
	}
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if string(existing) == string(payload) {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrImmutableResult, filepath.Base(path))
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return writeExclusive(path, payload, 0o640)
}

// SaveObservation stores an observation fact file for a candidate.
func (store *Store) SaveObservation(ctx context.Context, recordID, candidateID string, observation Observation) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.saveObservationLocked(ctx, recordID, candidateID, observation)
}

func (store *Store) saveObservationLocked(ctx context.Context, recordID, candidateID string, observation Observation) error {
	if err := readContextErr(ctx); err != nil {
		return err
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return err
	}
	candidateDir, err := store.candidateDir(recordDir, candidateID)
	if err != nil {
		return err
	}
	path := filepath.Join(candidateDir, "observation.json")
	if err := writeOnce(path, observation); err != nil {
		return err
	}
	payload, _ := marshalIndented(observation)
	return store.appendEvent(recordDir, "reviewer", "observation_recorded", candidateID,
		fmt.Sprintf("pcm_digest=%s frames=%d peak=%.3f dBFS rms=%.3f dBFS", observation.PCMDigest, observation.FrameCount, observation.PeakDBFS, observation.RMSDBFS),
		digestBytes(payload))
}

// LoadObservation reads a stored observation.
func (store *Store) LoadObservation(_ context.Context, recordID, candidateID string) (Observation, error) {
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return Observation{}, err
	}
	candidateDir, err := store.candidateDir(recordDir, candidateID)
	if err != nil {
		return Observation{}, err
	}
	var observation Observation
	if err := readJSONFile(filepath.Join(candidateDir, "observation.json"), &observation); err != nil {
		return Observation{}, err
	}
	return observation, nil
}

// SaveReconstruction stores the independent rebuild result.
func (store *Store) SaveReconstruction(ctx context.Context, recordID, candidateID string, reconstruction Reconstruction) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.saveReconstructionLocked(ctx, recordID, candidateID, reconstruction)
}

func (store *Store) saveReconstructionLocked(ctx context.Context, recordID, candidateID string, reconstruction Reconstruction) error {
	if err := readContextErr(ctx); err != nil {
		return err
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return err
	}
	candidateDir, err := store.candidateDir(recordDir, candidateID)
	if err != nil {
		return err
	}
	path := filepath.Join(candidateDir, "reconstruction.json")
	if err := writeOnce(path, reconstruction); err != nil {
		return err
	}
	payload, _ := marshalIndented(reconstruction)
	return store.appendEvent(recordDir, "reviewer", "reconstruction_recorded", candidateID,
		fmt.Sprintf("expected_frames=%d matches_download=%t max_residual=%.2f LSB", reconstruction.ExpectedFrames, reconstruction.MatchesDownloaded, reconstruction.MaxResidualLSB),
		digestBytes(payload))
}

// LoadReconstruction reads a stored reconstruction.
func (store *Store) LoadReconstruction(_ context.Context, recordID, candidateID string) (Reconstruction, error) {
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return Reconstruction{}, err
	}
	candidateDir, err := store.candidateDir(recordDir, candidateID)
	if err != nil {
		return Reconstruction{}, err
	}
	var reconstruction Reconstruction
	if err := readJSONFile(filepath.Join(candidateDir, "reconstruction.json"), &reconstruction); err != nil {
		return Reconstruction{}, err
	}
	return reconstruction, nil
}

// SaveVerdict stores a verdict keyed by candidate and rule set. A new rule
// set writes a new file; the same rule set can only be reproduced verbatim.
func (store *Store) SaveVerdict(ctx context.Context, recordID string, verdict Verdict) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.saveVerdictLocked(ctx, recordID, verdict)
}

func (store *Store) saveVerdictLocked(ctx context.Context, recordID string, verdict Verdict) error {
	if err := readContextErr(ctx); err != nil {
		return err
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return err
	}
	path := filepath.Join(store.verdictsDir(recordDir), verdictFileName(verdict.CandidateID, verdict.RuleSetID))
	if err := writeOnce(path, verdict); err != nil {
		return err
	}
	payload, _ := marshalIndented(verdict)
	return store.appendEvent(recordDir, "rule-engine", "verdict_recorded", verdict.CandidateID,
		fmt.Sprintf("rule_set=%s outcome=%s findings=%d", verdict.RuleSetID, verdict.Outcome, len(verdict.Findings)),
		digestBytes(payload))
}

func verdictFileName(candidateID, ruleSetID string) string {
	return sanitizeComponent(candidateID) + "." + sanitizeComponent(strings.ReplaceAll(ruleSetID, "/", "-")) + ".json"
}

// LoadVerdict reads a stored verdict for a candidate and rule set.
func (store *Store) LoadVerdict(_ context.Context, recordID, candidateID, ruleSetID string) (Verdict, error) {
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return Verdict{}, err
	}
	path := filepath.Join(store.verdictsDir(recordDir), verdictFileName(candidateID, ruleSetID))
	var verdict Verdict
	if err := readJSONFile(path, &verdict); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

// existingVerdict returns a previously recorded verdict for the same candidate
// and rule set. Review uses it to preserve the original conclusion instead of
// recomputing one: a decision already published must not change under the
// same rule set.
func (store *Store) existingVerdict(recordDir, candidateID, ruleSetID string) (Verdict, bool, error) {
	path := filepath.Join(store.verdictsDir(recordDir), verdictFileName(candidateID, ruleSetID))
	var verdict Verdict
	if err := readJSONFile(path, &verdict); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Verdict{}, false, nil
		}
		return Verdict{}, false, err
	}
	return verdict, true, nil
}

// DecisionInput records who decided what and why. The outcome is taken from
// the candidate's recorded verdict; there is no override field, because a
// decision that could contradict its own verdict would make the rules
// advisory rather than authoritative.
type DecisionInput struct {
	Actor       string
	CandidateID string
	Rationale   string
	Historical  bool
}

// RecordDecision appends a decision file. Decisions are append-only: a later
// third-party candidate adds a new decision, while the historical decision
// stays readable verbatim.
func (store *Store) RecordDecision(ctx context.Context, recordID string, input DecisionInput) (Decision, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := readContextErr(ctx); err != nil {
		return Decision{}, err
	}
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return Decision{}, err
	}
	if strings.TrimSpace(input.Actor) == "" {
		return Decision{}, errors.New("a decision requires an actor")
	}
	if strings.TrimSpace(input.Rationale) == "" {
		return Decision{}, errors.New("a decision requires a rationale")
	}
	if !recordIDPattern.MatchString(input.CandidateID) {
		return Decision{}, fmt.Errorf("candidate identifier %q is invalid", input.CandidateID)
	}
	verdict, err := store.loadLatestVerdictLocked(recordDir, input.CandidateID)
	if err != nil {
		if !input.Historical {
			return Decision{}, err
		}
		// A historical decision predates any candidate verdict: it records
		// that the delivery was accepted under the old process. It carries
		// the recorded outcome of "historical_accept" and cannot be edited
		// afterwards.
		verdict = Verdict{RuleSetID: RuleSetID, Outcome: OutcomeHistoricalAccept}
	}
	outcome := verdict.Outcome
	decisions, err := store.readDecisions(recordDir)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{
		Sequence:    len(decisions) + 1,
		DecidedAt:   store.clock(),
		Actor:       input.Actor,
		CandidateID: input.CandidateID,
		RuleSetID:   verdict.RuleSetID,
		Outcome:     outcome,
		Rationale:   input.Rationale,
		Historical:  input.Historical,
	}
	if err := ensureDir(store.decisionsDir(recordDir)); err != nil {
		return Decision{}, err
	}
	payload, err := marshalIndented(decision)
	if err != nil {
		return Decision{}, err
	}
	path := filepath.Join(store.decisionsDir(recordDir), fmt.Sprintf("%04d-%s.json", decision.Sequence, sanitizeComponent(input.CandidateID)))
	if err := writeExclusive(path, payload, 0o640); err != nil {
		return Decision{}, err
	}
	if err := store.appendEvent(recordDir, input.Actor, "decision_recorded", input.CandidateID,
		fmt.Sprintf("decision=%d outcome=%s historical=%t rationale=%s", decision.Sequence, outcome, input.Historical, input.Rationale),
		digestBytes(payload)); err != nil {
		return Decision{}, err
	}
	return decision, nil
}

func (store *Store) loadLatestVerdictLocked(recordDir, candidateID string) (Verdict, error) {
	paths, err := filepath.Glob(filepath.Join(store.verdictsDir(recordDir), sanitizeComponent(candidateID)+".*.json"))
	if err != nil {
		return Verdict{}, err
	}
	if len(paths) == 0 {
		return Verdict{}, fmt.Errorf("candidate %q has no verdict to decide on", candidateID)
	}
	// Prefer the current rule set when present; otherwise the lexically last
	// stored rule set is used so an older verdict remains decidable.
	preferred := filepath.Join(store.verdictsDir(recordDir), verdictFileName(candidateID, RuleSetID))
	if _, statErr := os.Stat(preferred); statErr == nil {
		paths = []string{preferred}
	}
	var verdict Verdict
	if err := readJSONFile(paths[len(paths)-1], &verdict); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

// LoadDecisions returns the append-only decision sequence.
func (store *Store) LoadDecisions(_ context.Context, recordID string) ([]Decision, error) {
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return nil, err
	}
	return store.readDecisions(recordDir)
}

func (store *Store) readDecisions(recordDir string) ([]Decision, error) {
	paths, err := filepath.Glob(filepath.Join(store.decisionsDir(recordDir), "*.json"))
	if err != nil {
		return nil, err
	}
	// Glob is lexical; the zero-padded sequence prefix keeps it ordered.
	decisions := make([]Decision, 0, len(paths))
	for _, path := range paths {
		var decision Decision
		if err := readJSONFile(path, &decision); err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	if len(decisions) != len(paths) {
		return nil, errors.New("decision log is incomplete")
	}
	for index, decision := range decisions {
		if decision.Sequence != index+1 {
			return nil, fmt.Errorf("decision %d has sequence %d", index, decision.Sequence)
		}
	}
	return decisions, nil
}

func readJSONFile(path string, target any) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

// ListedCandidates returns candidate IDs in arrival order.
func (store *Store) ListedCandidates(_ context.Context, recordID string) ([]string, error) {
	recordDir, err := store.recordDir(recordID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(recordDir, "candidates"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		ids = append(ids, entry.Name())
	}
	// Candidate folder names are arrival order only when names sort the same
	// way; expose the exact set and let the caller sort by creation time.
	return ids, nil
}

// RecheckCandidate re-verifies every stored evidence digest and re-reads the
// candidate description. It is the explicit "the historical evidence is still
// intact" statement a later review can rely on.
func (store *Store) RecheckCandidate(ctx context.Context, recordID, candidateID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := readContextErr(ctx); err != nil {
		return err
	}
	candidate, err := store.LoadCandidate(ctx, recordID, candidateID)
	if err != nil {
		return err
	}
	for _, evidence := range candidate.Evidence {
		if _, err := store.ReadEvidence(ctx, recordID, candidateID, evidence); err != nil {
			return err
		}
	}
	return store.verifyCandidateEvidence(ctx, recordID, candidate)
}
