package auditdemo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"voice-timeline-renderer/internal/audit"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/httpapi"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

const (
	baselineSourceA  = "audit_source_a"
	baselineSourceB  = "audit_source_b"
	baselineTimeline = "audit_timeline"
	baselineRender   = "audit_baseline_render"
	currentProcessor = "voice-timeline-renderer/production"

	// Target peak and tolerance live in the record, not in the demo; these
	// constants only describe the production timeline request.
	productionTargetPeakDBFS = -1.0
)

// Delivery is the historical production render that later candidates are
// measured against. Its bytes are the byte-level promise.
type Delivery struct {
	Digest        string
	Frames        int64
	PeakDBFS      float64
	SampleRate    int
	Channels      int
	PCM           []byte
	Receipt       []byte
	SourceBytes   map[string][]byte
	Spans         []domain.TimelineSpan
	AppliedGainDB float64
	TimelineID    string
	RenderID      string
}

// Runner owns a real service instance behind an in-process HTTP server.
type Runner struct {
	server  *httptest.Server
	http    *http.Client
	tempDir string
}

// NewRunner starts the production service on an isolated data directory.
func NewRunner(_ context.Context, auditRoot string) (*Runner, error) {
	dataDir, err := os.MkdirTemp(auditRoot, "production-*")
	if err != nil {
		return nil, err
	}
	repo, err := storage.NewRepository(dataDir)
	if err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, err
	}
	serviceLayer := service.New(repo, service.DefaultConfig())
	api := httpapi.New(serviceLayer, httpapi.Options{
		MaxSourceRequestBytes: 48 << 20,
		MaxJSONBytes:          2 << 20,
	})
	server := httptest.NewServer(api.Handler())
	return &Runner{
		server:  server,
		http:    &http.Client{Timeout: 60 * time.Second},
		tempDir: dataDir,
	}, nil
}

func (runner *Runner) Close() {
	runner.server.Close()
	_ = os.RemoveAll(runner.tempDir)
}

func (runner *Runner) put(ctx context.Context, path string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, runner.server.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return runner.do(request)
}

func (runner *Runner) get(ctx context.Context, path string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, runner.server.URL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	return runner.do(request)
}

func (runner *Runner) do(request *http.Request) (int, []byte, error) {
	response, err := runner.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, payload, nil
}

// sourceWAV builds one deterministic input source for the production chain.
func sourceWAV(rate, channels int, milliseconds int, fundamental, amplitude float64) ([]byte, error) {
	frames := rate * milliseconds / 1000
	samples := make([]float64, frames*channels)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(rate)
		envelope := 0.55 + 0.45*sin(2*pi*2.2*seconds)
		value := amplitude * envelope * sin(2*pi*fundamental*seconds)
		if channels == 1 {
			samples[frame] = value
			continue
		}
		samples[frame*2] = value
		samples[frame*2+1] = 0.8 * amplitude * envelope * sin(2*pi*fundamental*seconds+0.35)
	}
	return audit.EncodePCM16Wave(rate, channels, audit.Quantize16(samples, audit.QuantRoundHalfAway))
}

const pi = 3.141592653589793

// mustJSON encodes a value with stable indentation for evidence storage.
func mustJSON(value any) []byte {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		panic(err)
	}
	return payload
}

func sin(value float64) float64 { return math.Sin(value) }

// Baseline runs the production chain end to end and captures the delivered
// bytes plus every input a reviewer would need.
func (runner *Runner) Baseline(ctx context.Context) (*Delivery, error) {
	sourceABytes, err := sourceWAV(48000, 1, 120, 220, 0.30)
	if err != nil {
		return nil, err
	}
	sourceBBytes, err := sourceWAV(44100, 2, 100, 330, 0.25)
	if err != nil {
		return nil, err
	}
	for id, payload := range map[string][]byte{baselineSourceA: sourceABytes, baselineSourceB: sourceBBytes} {
		status, body, err := runner.put(ctx, "/v1/sources/"+id, service.SourceWriteRequest{
			Name:      "audit source " + id,
			WAVBase64: base64.StdEncoding.EncodeToString(payload),
		})
		if err != nil {
			return nil, err
		}
		if status != http.StatusCreated {
			return nil, fmt.Errorf("source %s registration returned %d: %s", id, status, body)
		}
	}

	spans := []domain.TimelineSpan{
		{SourceID: baselineSourceA, SourceStartFrame: 1200, SourceEndFrame: 4560, GainDB: -2, CrossfadeFrames: 0},
		{SourceID: baselineSourceB, SourceStartFrame: 400, SourceEndFrame: 2800, GainDB: 0.5, CrossfadeFrames: 480},
	}
	status, body, err := runner.put(ctx, "/v1/timelines/"+baselineTimeline, service.TimelineWriteRequest{
		Name:           "audit historical delivery",
		SampleRate:     48000,
		OutputChannels: 1,
		TargetPeakDBFS: productionTargetPeakDBFS,
		Spans:          spans,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("timeline creation returned %d: %s", status, body)
	}
	var plan domain.Timeline
	if err := json.Unmarshal(body, &plan); err != nil {
		return nil, err
	}

	status, body, err = runner.put(ctx, "/v1/renders/"+baselineRender, service.RenderWriteRequest{TimelineID: baselineTimeline})
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("render creation returned %d: %s", status, body)
	}
	var job domain.RenderJob
	if err := json.Unmarshal(body, &job); err != nil {
		return nil, err
	}
	if job.State != domain.RenderCompleted || job.Output == nil {
		return nil, fmt.Errorf("baseline render ended in state %s", job.State)
	}

	status, receiptBytes, err := runner.get(ctx, "/v1/renders/"+baselineRender+"/receipt")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("receipt download returned %d", status)
	}
	status, pcmBytes, err := runner.get(ctx, "/v1/renders/"+baselineRender+"/output")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("output download returned %d", status)
	}
	receiptEvidence, err := audit.ParseReceiptEvidence(receiptBytes)
	if err != nil {
		return nil, err
	}

	return &Delivery{
		Digest:        receiptEvidence.OutputDigest,
		Frames:        receiptEvidence.OutputFrameCount,
		PeakDBFS:      receiptEvidence.OutputPeakDBFS,
		SampleRate:    receiptEvidence.OutputSampleRate,
		Channels:      receiptEvidence.OutputChannels,
		PCM:           pcmBytes,
		Receipt:       receiptBytes,
		SourceBytes:   map[string][]byte{baselineSourceA: sourceABytes, baselineSourceB: sourceBBytes},
		Spans:         spans,
		AppliedGainDB: receiptEvidence.AppliedGainDB,
		TimelineID:    baselineTimeline,
		RenderID:      baselineRender,
	}, nil
}

// BaselineCandidate renders the delivery as an audit candidate declaration.
func (delivery *Delivery) BaselineCandidate() audit.Candidate {
	return audit.Candidate{
		ID:               baselineRender,
		Role:             "historical_delivery",
		CreatedAt:        "2026-01-15T09:00:00Z",
		ProcessorVersion: currentProcessor,
		TimelineID:       delivery.TimelineID,
		SampleRate:       delivery.SampleRate,
		OutputChannels:   delivery.Channels,
		Post: audit.PostProcessing{
			NormalizationTargetDBFS: productionTargetPeakDBFS,
			AppliedGainDB:           delivery.AppliedGainDB,
			Quantization:            audit.QuantRoundHalfAway,
		},
		Spans: declaredSpans(delivery),
		Evidence: []audit.Evidence{
			{Kind: audit.EvidenceDownloadedPCM, Subject: "output", RelativePath: "download/output.wav",
				SHA256: audit.DigestBytes(delivery.PCM), ByteCount: int64(len(delivery.PCM)), MediaType: "audio/wav",
				Description: "historical delivered PCM"},
			{Kind: audit.EvidenceReceipt, Subject: "receipt", RelativePath: "download/receipt.json",
				SHA256: audit.DigestBytes(delivery.Receipt), ByteCount: int64(len(delivery.Receipt)), MediaType: "application/json",
				Description: "historical production receipt"},
			{Kind: audit.EvidenceSourceWAV, Subject: baselineSourceA, RelativePath: "sources/" + baselineSourceA + ".wav",
				SHA256: audit.DigestBytes(delivery.SourceBytes[baselineSourceA]), ByteCount: int64(len(delivery.SourceBytes[baselineSourceA]))},
			{Kind: audit.EvidenceSourceWAV, Subject: baselineSourceB, RelativePath: "sources/" + baselineSourceB + ".wav",
				SHA256: audit.DigestBytes(delivery.SourceBytes[baselineSourceB]), ByteCount: int64(len(delivery.SourceBytes[baselineSourceB]))},
		},
	}
}

// BaselineArtifacts carries the bytes that BaselineCandidate describes.
func (delivery *Delivery) BaselineArtifacts() []audit.Artifact {
	return []audit.Artifact{
		{Kind: audit.EvidenceDownloadedPCM, Subject: "output", Extension: "wav", MediaType: "audio/wav",
			Description: "historical delivered PCM", Bytes: delivery.PCM},
		{Kind: audit.EvidenceReceipt, Subject: "receipt", Extension: "json", MediaType: "application/json",
			Description: "historical production receipt", Bytes: delivery.Receipt},
		{Kind: audit.EvidenceSourceWAV, Subject: baselineSourceA, Extension: "wav", MediaType: "audio/wav",
			Bytes: delivery.SourceBytes[baselineSourceA]},
		{Kind: audit.EvidenceSourceWAV, Subject: baselineSourceB, Extension: "wav", MediaType: "audio/wav",
			Bytes: delivery.SourceBytes[baselineSourceB]},
	}
}

func declaredSpans(delivery *Delivery) []audit.CandidateSpan {
	return []audit.CandidateSpan{
		{
			Index: 0,
			Selection: audit.Selection{
				SourceID: baselineSourceA, SourceDigest: audit.DigestBytes(delivery.SourceBytes[baselineSourceA]),
				SourceStartFrame: 1200, SourceEndFrame: 4560,
				SourceSampleRate: 48000, SourceChannels: 1,
			},
			Processing: audit.SpanProcessing{GainDB: -2, CrossfadeFrames: 0,
				ResampleMethod: audit.ResampleLinear, ChannelMapping: audit.MappingUnchanged},
		},
		{
			Index: 1,
			Selection: audit.Selection{
				SourceID: baselineSourceB, SourceDigest: audit.DigestBytes(delivery.SourceBytes[baselineSourceB]),
				SourceStartFrame: 400, SourceEndFrame: 2800,
				SourceSampleRate: 44100, SourceChannels: 2,
			},
			Processing: audit.SpanProcessing{GainDB: 0.5, CrossfadeFrames: 480,
				ResampleMethod: audit.ResampleLinear, ChannelMapping: audit.MappingStereoToMono},
		},
	}
}
