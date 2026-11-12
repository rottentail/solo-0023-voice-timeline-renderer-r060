package audit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// PCM is the independent, integer-domain view of a 16-bit RIFF/WAVE payload.
// The audit path deliberately does not call the production codec: an
// independent reviewer must be able to re-derive measurements from bytes.
type PCM struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	FrameCount    int64
	Samples       []int16
	ByteCount     int64
}

// DecodePCM16Wave parses RIFF/WAVE with a 16-bit integer PCM fmt chunk. It
// accepts mono and stereo only, rejects anything not frame-aligned, and never
// guesses at a partially readable payload.
func DecodePCM16Wave(payload []byte) (*PCM, error) {
	if len(payload) < 12 {
		return nil, errors.New("payload is shorter than a RIFF header")
	}
	if string(payload[0:4]) != "RIFF" || string(payload[8:12]) != "WAVE" {
		return nil, errors.New("payload is not RIFF/WAVE")
	}
	riffSize := uint64(binary.LittleEndian.Uint32(payload[4:8]))
	if riffSize+8 > uint64(len(payload)) {
		return nil, errors.New("RIFF size exceeds payload")
	}

	var fmtChunk, dataChunk []byte
	foundFmt, foundData := false, false
	offset := 12
	for offset+8 <= len(payload) {
		chunkID := string(payload[offset : offset+4])
		chunkSize := int64(binary.LittleEndian.Uint32(payload[offset+4 : offset+8]))
		start := int64(offset + 8)
		end := start + chunkSize
		if chunkSize < 0 || end < start || end > int64(len(payload)) {
			return nil, fmt.Errorf("chunk %q exceeds payload", chunkID)
		}
		switch chunkID {
		case "fmt ":
			if !foundFmt {
				fmtChunk = payload[start:end]
				foundFmt = true
			}
		case "data":
			if !foundData {
				dataChunk = payload[start:end]
				foundData = true
			}
		}
		offset = int(end)
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if !foundFmt || len(fmtChunk) < 16 {
		return nil, errors.New("fmt chunk is missing or short")
	}
	if !foundData {
		return nil, errors.New("data chunk is missing")
	}

	format := binary.LittleEndian.Uint16(fmtChunk[0:2])
	channels := int(binary.LittleEndian.Uint16(fmtChunk[2:4]))
	rate := int(binary.LittleEndian.Uint32(fmtChunk[4:8]))
	byteRate := binary.LittleEndian.Uint32(fmtChunk[8:12])
	blockAlign := int(binary.LittleEndian.Uint16(fmtChunk[12:14]))
	bits := int(binary.LittleEndian.Uint16(fmtChunk[14:16]))
	if format != 1 {
		return nil, fmt.Errorf("unsupported format code %d", format)
	}
	if bits != 16 {
		return nil, fmt.Errorf("unsupported bit width %d", bits)
	}
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("unsupported channel count %d", channels)
	}
	if rate < 8000 || rate > 192000 {
		return nil, fmt.Errorf("sample rate %d outside accepted range", rate)
	}
	if blockAlign != channels*2 {
		return nil, errors.New("block alignment is inconsistent")
	}
	if byteRate != uint32(rate*blockAlign) {
		return nil, errors.New("byte rate is inconsistent")
	}
	if len(dataChunk) == 0 || len(dataChunk)%blockAlign != 0 {
		return nil, errors.New("sample data is empty or not frame-aligned")
	}

	samples := make([]int16, len(dataChunk)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(dataChunk[index*2 : index*2+2]))
	}
	return &PCM{
		SampleRate:    rate,
		Channels:      channels,
		BitsPerSample: bits,
		FrameCount:    int64(len(samples) / channels),
		Samples:       samples,
		ByteCount:     int64(len(payload)),
	}, nil
}

// EncodePCM16Wave writes a canonical 44-byte-header 16-bit PCM WAV.
func EncodePCM16Wave(rate, channels int, samples []int16) ([]byte, error) {
	if rate < 8000 || rate > 192000 {
		return nil, errors.New("sample rate outside accepted range")
	}
	if channels != 1 && channels != 2 {
		return nil, errors.New("channel count must be one or two")
	}
	if len(samples) == 0 || len(samples)%channels != 0 {
		return nil, errors.New("samples must be non-empty and frame-aligned")
	}
	dataBytes := len(samples) * 2
	result := make([]byte, 44+dataBytes)
	copy(result[0:4], "RIFF")
	binary.LittleEndian.PutUint32(result[4:8], uint32(len(result)-8))
	copy(result[8:12], "WAVE")
	copy(result[12:16], "fmt ")
	binary.LittleEndian.PutUint32(result[16:20], 16)
	binary.LittleEndian.PutUint16(result[20:22], 1)
	binary.LittleEndian.PutUint16(result[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(result[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(result[28:32], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(result[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(result[34:36], 16)
	copy(result[36:40], "data")
	binary.LittleEndian.PutUint32(result[40:44], uint32(dataBytes))
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(result[44+index*2:46+index*2], uint16(sample))
	}
	return result, nil
}

// Quantize16 converts normalized floats into 16-bit integer samples using the
// documented review convention: negative samples scale by 32768 and
// non-negative samples scale by 32767, then saturate.
func Quantize16(samples []float64, method string) []int16 {
	result := make([]int16, len(samples))
	for index, sample := range samples {
		clamped := clampSample(sample)
		switch method {
		case QuantTruncateTowardZero:
			result[index] = quantizeTruncate(clamped)
		case QuantBit14Truncate:
			result[index] = int16(uint16(quantizeOne(clamped)) & 0xFFFC)
		default:
			result[index] = quantizeOne(clamped)
		}
	}
	return result
}

func quantizeOne(sample float64) int16 {
	switch {
	case sample <= -1:
		return -32768
	case sample >= 1:
		return 32767
	case sample < 0:
		return int16(roundHalfAway(sample * 32768))
	default:
		return int16(roundHalfAway(sample * 32767))
	}
}

func quantizeTruncate(sample float64) int16 {
	switch {
	case sample <= -1:
		return -32768
	case sample >= 1:
		return 32767
	case sample < 0:
		return int16(int64(sample * 32768))
	default:
		return int16(int64(sample * 32767))
	}
}

func roundHalfAway(value float64) int64 {
	if value >= 0 {
		return int64(value + 0.5)
	}
	return int64(value - 0.5)
}

func clampSample(sample float64) float64 {
	if math.IsNaN(sample) {
		return 0
	}
	if sample > 1 {
		return 1
	}
	if sample < -1 {
		return -1
	}
	return sample
}

// normalizedSample converts integer PCM to the review-domain amplitude
// convention: negative samples use 32768, non-negative use 32767.
func normalizedSample(value int16) float64 {
	if value < 0 {
		return float64(value) / 32768.0
	}
	return float64(value) / 32767.0
}
