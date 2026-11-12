package auditdemo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
)

func (runner *Runner) putSource(ctx context.Context, id string, payload []byte) error {
	status, body, err := runner.put(ctx, "/v1/sources/"+id, service.SourceWriteRequest{
		Name:      "audit source " + id,
		WAVBase64: base64.StdEncoding.EncodeToString(payload),
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fmt.Errorf("source %s registration returned %d: %s", id, status, body)
	}
	return nil
}

// timelineSpanRequest mirrors the wire shape of a timeline span.
type timelineSpanRequest struct {
	SourceID         string  `json:"source_id"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	GainDB           float64 `json:"gain_db"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
}

type timelineRequest struct {
	Name           string                `json:"name"`
	SampleRate     int                   `json:"sample_rate"`
	OutputChannels int                   `json:"output_channels"`
	TargetPeakDBFS float64               `json:"target_peak_dbfs"`
	Spans          []timelineSpanRequest `json:"spans"`
}

func (runner *Runner) putTimeline(ctx context.Context, id string, spans []struct {
	ID     string
	Start  int64
	End    int64
	GainDB float64
	Cross  int64
}) (domain.Timeline, error) {
	request := timelineRequest{
		Name:           "audit candidate " + id,
		SampleRate:     48000,
		OutputChannels: 1,
		TargetPeakDBFS: productionTargetPeakDBFS,
	}
	for _, span := range spans {
		request.Spans = append(request.Spans, timelineSpanRequest{
			SourceID:         span.ID,
			SourceStartFrame: span.Start,
			SourceEndFrame:   span.End,
			GainDB:           span.GainDB,
			CrossfadeFrames:  span.Cross,
		})
	}
	status, body, err := runner.put(ctx, "/v1/timelines/"+id, request)
	if err != nil {
		return domain.Timeline{}, err
	}
	if status != http.StatusCreated {
		return domain.Timeline{}, fmt.Errorf("timeline %s creation returned %d: %s", id, status, body)
	}
	var plan domain.Timeline
	if err := json.Unmarshal(body, &plan); err != nil {
		return domain.Timeline{}, err
	}
	return plan, nil
}

func (runner *Runner) putRender(ctx context.Context, id, timelineID string) (domain.RenderJob, error) {
	status, body, err := runner.put(ctx, "/v1/renders/"+id, service.RenderWriteRequest{TimelineID: timelineID})
	if err != nil {
		return domain.RenderJob{}, err
	}
	if status != http.StatusCreated {
		return domain.RenderJob{}, fmt.Errorf("render %s creation returned %d: %s", id, status, body)
	}
	var job domain.RenderJob
	if err := json.Unmarshal(body, &job); err != nil {
		return domain.RenderJob{}, err
	}
	return job, nil
}

func (runner *Runner) getReceipt(ctx context.Context, renderID string) ([]byte, error) {
	status, body, err := runner.get(ctx, "/v1/renders/"+renderID+"/receipt")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("receipt %s returned %d: %s", renderID, status, body)
	}
	return body, nil
}

func (runner *Runner) getOutput(ctx context.Context, renderID string) ([]byte, error) {
	status, body, err := runner.get(ctx, "/v1/renders/"+renderID+"/output")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("output %s returned %d: %s", renderID, status, body)
	}
	return body, nil
}
