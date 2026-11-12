package audit

import (
	"math"
	"sort"
)

// Fixed analysis constants. These are part of the rule set: changing them
// requires a new RuleSetID so that older verdicts stay interpretable.
const (
	maxAnalysisFrames = 65536
	segmentFrames     = 1024
	segmentPositions  = 5
	minSearchLag      = 64
	// PhaseToleranceSamples is the largest constant delay that still counts
	// as sample-aligned. Half a sample keeps a phase shift attributable and
	// bounded instead of silently sliding the timeline.
	PhaseToleranceSamples = 0.5
	// LagDriftToleranceSamples is the largest change in lag across the
	// duration that still counts as the same timebase.
	LagDriftToleranceSamples = 0.25
	// ActiveSampleThreshold is one 16-bit LSB; quieter samples are silence.
	ActiveSampleThreshold = 1.0 / 32767.0
	silenceFloorDBFS      = -120.0
)

// lagEstimate is the best normalized cross-correlation lag between two
// sequences sampled at the same rate. A positive lag means y[index+lag]
// aligns with x[index].
type lagEstimate struct {
	Lag         float64
	Correlation float64
	Valid       bool
}

// measureLagSigned finds the lag of largest signed correlation: the classic
// constant-delay estimate. A polarity-inverted signal shows up as a strongly
// negative peak.
func measureLagSigned(x, y []float64, maxLag, window int) lagEstimate {
	estimate, values, _ := scanLag(x, y, maxLag, window, false)
	if !estimate.Valid {
		return estimate
	}
	center := int(estimate.Lag) + maxLag
	return refineLag(estimate, values, center)
}

// measureLagMagnitude finds the lag of largest magnitude correlation. It
// recovers the true delay of a polarity-inverted signal, where the signed
// search would latch onto a spurious positive lobe.
func measureLagMagnitude(x, y []float64, maxLag, window int) lagEstimate {
	estimate, values, _ := scanLag(x, y, maxLag, window, true)
	if !estimate.Valid {
		return estimate
	}
	center := int(estimate.Lag) + maxLag
	return refineLag(estimate, values, center)
}

func scanLag(x, y []float64, maxLag, window int, magnitude bool) (lagEstimate, []float64, bool) {
	if window > len(x) {
		window = len(x)
	}
	if window > len(y) {
		window = len(y)
	}
	if window < 256 {
		return lagEstimate{}, nil, false
	}
	x = x[:window]
	y = y[:window]
	if maxLag >= len(y) {
		maxLag = len(y) - 1
	}
	best := -2.0
	bestLag := 0
	values := make([]float64, 0, 2*maxLag+1)
	for lag := -maxLag; lag <= maxLag; lag++ {
		value := correlationAt(x, y, lag)
		values = append(values, value)
		score := value
		if magnitude {
			score = math.Abs(value)
		}
		if score > best {
			best = score
			bestLag = lag
		}
	}
	if best < 0 {
		return lagEstimate{}, values, false
	}
	return lagEstimate{Lag: float64(bestLag), Correlation: values[bestLag+maxLag], Valid: true}, values, true
}

func refineLag(estimate lagEstimate, values []float64, center int) lagEstimate {
	if center <= 0 || center+1 >= len(values) {
		return estimate
	}
	left := math.Abs(values[center-1])
	middle := math.Abs(values[center])
	right := math.Abs(values[center+1])
	denominator := left - 2*middle + right
	if math.Abs(denominator) > 1e-18 && middle >= left && middle >= right {
		offset := 0.5 * (left - right) / denominator
		if math.Abs(offset) <= 0.5 {
			estimate.Lag += offset
		}
	}
	return estimate
}

func correlationAt(x, y []float64, lag int) float64 {
	start := 0
	if lag < 0 {
		start = -lag
	}
	end := len(x)
	if edge := len(y) - lag; edge < end {
		end = edge
	}
	if start >= end {
		return 0
	}
	xy, xx, yy := 0.0, 0.0, 0.0
	for index := start; index < end; index++ {
		a := x[index]
		b := y[index+lag]
		xy += a * b
		xx += a * a
		yy += b * b
	}
	if xx <= 0 || yy <= 0 {
		return 0
	}
	return xy / math.Sqrt(xx*yy)
}

type segmentScan struct {
	Start int
	Lag   float64
	Corr  float64
}

func scanSegments(reference, candidate []float64, maxLag int) ([]segmentScan, bool) {
	length := len(reference)
	if len(candidate) < length {
		length = len(candidate)
	}
	if length < segmentFrames*2 {
		return nil, false
	}
	scans := make([]segmentScan, 0, segmentPositions)
	for position := 0; position < segmentPositions; position++ {
		start := 0
		if segmentPositions > 1 {
			start = (length - segmentFrames) * position / (segmentPositions - 1)
		}
		estimate := measureLagSigned(reference[start:], candidate[start:], maxLag, segmentFrames)
		if !estimate.Valid || estimate.Correlation < 0.5 {
			continue
		}
		scans = append(scans, segmentScan{Start: start, Lag: estimate.Lag, Corr: estimate.Correlation})
	}
	return scans, len(scans) >= 2
}

func segmentLagDrift(scans []segmentScan) float64 {
	if len(scans) < 2 {
		return 0
	}
	lowest := scans[0].Lag
	highest := scans[0].Lag
	for _, scan := range scans[1:] {
		if scan.Lag < lowest {
			lowest = scan.Lag
		}
		if scan.Lag > highest {
			highest = scan.Lag
		}
	}
	return highest - lowest
}

func medianLag(scans []segmentScan) float64 {
	values := make([]float64, 0, len(scans))
	for _, scan := range scans {
		values = append(values, scan.Lag)
	}
	if len(values) == 0 {
		return math.NaN()
	}
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 0 {
		return (values[middle-1] + values[middle]) / 2
	}
	return values[middle]
}

// residualStats compares a candidate against an aligned reference and returns
// differences in 16-bit LSB units after removing the best-fit scalar gain.
type residualStats struct {
	Max     float64
	Mean    float64
	Over    int64
	Count   int64
	FitGain float64
}

func alignedResidual(reference, candidate []float64, lag, toleranceLSB float64) residualStats {
	shifted := shiftAndInterpolate(candidate, lag)
	length := len(reference)
	if len(shifted) < length {
		length = len(shifted)
	}
	start := 8
	end := length - 8
	if end <= start {
		return residualStats{FitGain: 1}
	}
	if end-start > maxAnalysisFrames {
		end = start + maxAnalysisFrames
	}
	cross, variance := 0.0, 0.0
	for index := start; index < end; index++ {
		cross += reference[index] * shifted[index]
		variance += shifted[index] * shifted[index]
	}
	fitted := 1.0
	if variance > 1e-18 {
		fitted = cross / variance
	}
	if fitted == 0 || math.IsNaN(fitted) || math.IsInf(fitted, 0) {
		fitted = 1
	}
	maxAbs, sumAbs := 0.0, 0.0
	var over, count int64
	for index := start; index < end; index++ {
		difference := math.Abs(reference[index]-shifted[index]*fitted) * 32767.0
		if difference > maxAbs {
			maxAbs = difference
		}
		sumAbs += difference
		if difference > toleranceLSB {
			over++
		}
		count++
	}
	if count == 0 {
		return residualStats{FitGain: fitted}
	}
	return residualStats{Max: maxAbs, Mean: sumAbs / float64(count), Over: over, Count: count, FitGain: fitted}
}

func shiftAndInterpolate(samples []float64, lag float64) []float64 {
	result := make([]float64, len(samples))
	for index := range result {
		position := float64(index) + lag
		if position < 0 {
			result[index] = 0
			continue
		}
		left := int(math.Floor(position))
		if left >= len(samples) {
			result[index] = 0
			continue
		}
		right := left + 1
		if right >= len(samples) {
			result[index] = samples[left]
			continue
		}
		weight := position - float64(left)
		result[index] = samples[left] + (samples[right]-samples[left])*weight
	}
	return result
}
