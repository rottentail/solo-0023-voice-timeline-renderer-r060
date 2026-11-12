package auditdemo

import (
	"context"
	"fmt"
	"math"

	"voice-timeline-renderer/internal/audit"
)

// SignalPlan describes a deterministic synthetic narration phrase. Every
// scenario derives its audio from the same plan so the only variable under
// test is the transformation.
type SignalPlan struct {
	SampleRate int
	Frames     int
	Channels   int
}

// generateSignal returns a review-domain float signal with a chirped tone plus
// a shape change around the middle so that a lag estimate is unambiguous and
// a drift across time is detectable.
func generateSignal(plan SignalPlan) []float64 {
	samples := make([]float64, plan.Frames*plan.Channels)
	for frame := 0; frame < plan.Frames; frame++ {
		seconds := float64(frame) / float64(plan.SampleRate)
		// Chirp from 330 Hz to 660 Hz plus a low harmonic. A change of
		// instantaneous frequency makes an unmixed timebase change obvious.
		frequency := 330 + 330*seconds/float64(plan.Frames)/float64(plan.SampleRate)
		primary := 0.55 * math.Sin(2*math.Pi*frequency*seconds)
		secondary := 0.25 * math.Sin(2*math.Pi*(frequency/2)*seconds+0.8)
		envelope := 0.5 + 0.5*math.Sin(2*math.Pi*1.5*seconds)
		value := (primary + secondary) * envelope
		if plan.Channels == 1 {
			samples[frame] = value
			continue
		}
		samples[frame*2] = value
		samples[frame*2+1] = 0.85 * value
	}
	return samples
}

// signalWAV encodes a generated signal as canonical 16-bit PCM.
func signalWAV(plan SignalPlan) ([]byte, error) {
	samples := generateSignal(plan)
	return audit.EncodePCM16Wave(plan.SampleRate, plan.Channels, audit.Quantize16(samples, audit.QuantRoundHalfAway))
}

// scenarioTransform applies a named, auditable transformation to a float
// signal. Each transform declares exactly one cause so the rule engine's
// classification can be checked against the intended cause.
type scenarioTransform struct {
	frameshift     float64
	clockRatio     float64
	quantization   string
	levelDB        float64
	polarityInvert bool
	addNoise       float64
	ditherLSB      float64
	truncate       bool
	dropFrames     int
}

func applyTransform(samples []float64, channels int, transform scenarioTransform) []float64 {
	result := make([]float64, len(samples))
	copy(result, samples)
	if transform.addNoise != 0 {
		for index := range result {
			// Deterministic noise so a re-run produces the same bytes.
			result[index] += transform.addNoise * math.Sin(float64(index)*1.6180339887)
		}
	}
	if transform.levelDB != 0 {
		gain := math.Pow(10, transform.levelDB/20)
		for index := range result {
			result[index] *= gain
		}
	}
	if transform.polarityInvert {
		for index := range result {
			result[index] = -result[index]
		}
	}
	if transform.frameshift != 0 {
		result = shift(result, channels, transform.frameshift)
	}
	if transform.clockRatio != 0 && transform.clockRatio != 1 {
		result = stretch(result, channels, transform.clockRatio)
	}
	if transform.dropFrames > 0 {
		// Drop trailing frames to simulate an early stop; length changes.
		frames := len(result) / channels
		keep := frames - transform.dropFrames
		if keep < 0 {
			keep = 0
		}
		result = result[:keep*channels]
	}
	if transform.ditherLSB != 0 {
		// Deterministic dither below half an LSB: requantization can change
		// the stored integer by at most one count, so any difference this
		// introduces belongs to the quantization class by construction.
		for index := range result {
			noise := math.Sin(float64(index)*12.9898) * 43758.5453
			fraction := noise - math.Floor(noise)
			result[index] += transform.ditherLSB * (2*fraction - 1) / 32767.0
		}
	}
	if transform.truncate {
		samples := audit.Quantize16(result, audit.QuantTruncateTowardZero)
		result = make([]float64, len(samples))
		for index, sample := range samples {
			if sample < 0 {
				result[index] = float64(sample) / 32768
			} else {
				result[index] = float64(sample) / 32767
			}
		}
	}
	return result
}

func shift(samples []float64, channels int, frames float64) []float64 {
	result := make([]float64, len(samples))
	for frame := 0; frame < len(samples)/channels; frame++ {
		position := float64(frame) - frames
		left := int(math.Floor(position))
		weight := position - float64(left)
		for channel := 0; channel < channels; channel++ {
			first := 0.0
			if left >= 0 && left < len(samples)/channels {
				first = samples[left*channels+channel]
			}
			second := 0.0
			if left+1 >= 0 && left+1 < len(samples)/channels {
				second = samples[(left+1)*channels+channel]
			}
			result[frame*channels+channel] = first + (second-first)*weight
		}
	}
	return result
}

func stretch(samples []float64, channels int, ratio float64) []float64 {
	if ratio <= 0 {
		ratio = 1
	}
	frames := len(samples) / channels
	targetFrames := int(float64(frames) * ratio)
	result := make([]float64, targetFrames*channels)
	for frame := 0; frame < targetFrames; frame++ {
		position := float64(frame) / ratio
		left := int(math.Floor(position))
		if left >= frames-1 {
			left = frames - 1
		}
		if left < 0 {
			left = 0
		}
		right := left + 1
		if right >= frames {
			right = frames - 1
		}
		weight := position - float64(left)
		for channel := 0; channel < channels; channel++ {
			first := samples[left*channels+channel]
			second := samples[right*channels+channel]
			result[frame*channels+channel] = first + (second-first)*weight
		}
	}
	return result
}

func transformWAV(plan SignalPlan, transform scenarioTransform) ([]byte, error) {
	samples := applyTransform(generateSignal(plan), plan.Channels, transform)
	quantization := transform.quantization
	if quantization == "" {
		quantization = audit.QuantRoundHalfAway
	}
	return audit.EncodePCM16Wave(plan.SampleRate, plan.Channels, audit.Quantize16(samples, quantization))
}

func describeTransform(transform scenarioTransform) string {
	return fmt.Sprintf(
		"frameshift=%.3f clock_ratio=%.6f quantization=%s level_db=%.3f polarity_invert=%t noise=%.6f drop_frames=%d",
		transform.frameshift, transform.clockRatio, transform.quantization, transform.levelDB,
		transform.polarityInvert, transform.addNoise, transform.dropFrames,
	)
}

var _ = context.Background
