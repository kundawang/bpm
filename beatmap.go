package bpm

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Sentinel errors returned by the variable-tempo analyser. Callers can use
// errors.Is to distinguish them from generic failures.
var (
	// ErrInvalidSampleRate is returned when Config.SampleRate is zero,
	// negative, NaN or infinite.
	ErrInvalidSampleRate = errors.New("bpm: invalid sample rate")
	// ErrInvalidConfig is returned when the tempo range or analysis window
	// configuration is unusable.
	ErrInvalidConfig = errors.New("bpm: invalid beat map configuration")
	// ErrAudioTooShort is returned when the input is shorter than
	// MinDuration; a beat map cannot be estimated from so little audio.
	ErrAudioTooShort = errors.New("bpm: audio shorter than the analysable minimum")
	// ErrNonFiniteSample is returned when the PCM input contains a NaN or
	// infinite sample.
	ErrNonFiniteSample = errors.New("bpm: non-finite PCM sample")
)

const (
	// MinDuration is the shortest piece of audio AnalyzeBeatMap accepts.
	// Anything shorter is rejected with ErrAudioTooShort rather than
	// producing a meaningless estimate.
	MinDuration = 2 * time.Second

	defaultMinBPM           = 60.0
	defaultMaxBPM           = 200.0
	defaultWindowSec        = 10.0
	defaultSteps            = 1024
	defaultSamples          = 256
	referenceProbes         = 8
	changeThreshold         = 0.08 // relative BPM deviation that opens a new segment
	ambiguityThreshold      = 0.10 // score margin that flags double/half tempo
	minWindowSec            = 1.0
	silenceFloorRatio       = 0.02
	weakConfidenceThreshold = 0.35
	// DuplicateTolerance is the smallest allowed gap between two reported
	// beats, in seconds. Beat grids on either side of a tempo change closer
	// than this are treated as the same beat.
	DuplicateTolerance = 0.08
)

// Config configures the variable-tempo beat map analysis. The zero value is
// not valid; use DefaultConfig and override the fields you care about.
type Config struct {
	// SampleRate of the PCM input in Hz. Must be positive and finite.
	SampleRate float64
	// MinBPM / MaxBPM bound the tempo search range. MinBPM must be strictly
	// smaller than MaxBPM and both must be positive and finite.
	MinBPM float64
	MaxBPM float64
	// Window is the length of each local tempo analysis window.
	Window time.Duration
	// Hop is the advance between consecutive analysis windows. It must be
	// positive and no larger than Window.
	Hop time.Duration
	// Steps is the number of tempo candidates scanned per window.
	Steps int
	// Samples is the number of deterministic probe positions used to score
	// each tempo candidate.
	Samples int
}

// DefaultConfig returns the recommended configuration: 44.1kHz input, a
// 60-200 BPM search range and 10 second analysis windows advancing by 5
// seconds.
func DefaultConfig() Config {
	return Config{
		SampleRate: float64(RATE),
		MinBPM:     defaultMinBPM,
		MaxBPM:     defaultMaxBPM,
		Window:     10 * time.Second,
		Hop:        5 * time.Second,
		Steps:      defaultSteps,
		Samples:    defaultSamples,
	}
}

// Beat is a single beat position on the time line.
type Beat struct {
	// Time of the beat in seconds from the start of the audio.
	Time float64 `json:"time"`
	// Confidence in [0,1]. Silent beats keep a small but non-zero value so
	// callers can keep drawing the grid rather than having gaps.
	Confidence float64 `json:"confidence"`
	// Weak marks beats whose local onset strength is well below the
	// surrounding material.
	Weak bool `json:"weak"`
	// Silent marks beats that fall inside a region at or below the silence
	// floor (2% of the peak envelope energy).
	Silent bool `json:"silent"`
}

// TempoSegment describes a stretch of audio with a single local tempo.
// Adjacent segments share their boundary time and together cover the whole
// duration of the input.
type TempoSegment struct {
	// Start / End are the segment bounds in seconds; the end is exclusive.
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	// BPM is the median local tempo estimate over the segment.
	BPM float64 `json:"bpm"`
	// Confidence in [0,1] describes how strongly the tempo trough stands
	// out from the rest of the search range. It is zero on silence.
	Confidence float64 `json:"confidence"`
	// Ambiguous is true when a double- or half-time tempo scored nearly as
	// well as BPM. The competing candidates are listed in Candidates.
	Ambiguous bool `json:"ambiguous"`
	// Candidates contains BPM and its in-range double/half harmonics.
	Candidates []float64 `json:"candidates,omitempty"`
}

// BeatMap is the result of a variable-tempo analysis. Beats are sorted by
// time and strictly increasing; segments are sorted by start time and
// contiguous.
type BeatMap struct {
	// Duration of the analysed audio in seconds.
	Duration float64        `json:"duration"`
	Beats    []Beat         `json:"beats"`
	Segments []TempoSegment `json:"segments"`
}

func isFinite(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0)
}

func (c Config) validated() (Config, error) {
	d := DefaultConfig()

	if c.SampleRate == 0 {
		c.SampleRate = d.SampleRate
	}
	if !isFinite(c.SampleRate) || c.SampleRate <= 0 {
		return c, fmt.Errorf("%w: sample rate must be a positive finite number, got %v", ErrInvalidSampleRate, c.SampleRate)
	}

	if c.MinBPM == 0 {
		c.MinBPM = d.MinBPM
	}
	if c.MaxBPM == 0 {
		c.MaxBPM = d.MaxBPM
	}
	if !isFinite(c.MinBPM) || !isFinite(c.MaxBPM) || c.MinBPM <= 0 || c.MaxBPM <= 0 {
		return c, fmt.Errorf("%w: BPM bounds must be positive and finite, got min=%v max=%v", ErrInvalidConfig, c.MinBPM, c.MaxBPM)
	}
	if c.MinBPM >= c.MaxBPM {
		return c, fmt.Errorf("%w: MinBPM (%v) must be strictly smaller than MaxBPM (%v)", ErrInvalidConfig, c.MinBPM, c.MaxBPM)
	}
	if c.MaxBPM >= 1000 {
		return c, fmt.Errorf("%w: MaxBPM %v is implausibly high", ErrInvalidConfig, c.MaxBPM)
	}

	if c.Window == 0 {
		c.Window = d.Window
	}
	if c.Window < minWindowSec*time.Second {
		return c, fmt.Errorf("%w: analysis window %s is shorter than the %s minimum", ErrInvalidConfig, c.Window, time.Duration(minWindowSec*float64(time.Second)))
	}
	if c.Hop == 0 {
		c.Hop = c.Window / 2
	}
	if c.Hop <= 0 || c.Hop > c.Window {
		return c, fmt.Errorf("%w: hop %s must be in (0, window=%s]", ErrInvalidConfig, c.Hop, c.Window)
	}

	if c.Steps == 0 {
		c.Steps = d.Steps
	}
	if c.Samples == 0 {
		c.Samples = d.Samples
	}
	if c.Steps <= 0 || c.Samples <= 0 {
		return c, fmt.Errorf("%w: Steps and Samples must be positive, got steps=%d samples=%d", ErrInvalidConfig, c.Steps, c.Samples)
	}
	return c, nil
}

// bpmToIntervalAt converts a tempo to a beat interval in energy-envelope
// units for an arbitrary sample rate.
func bpmToIntervalAt(bpm, sampleRate float64) float64 {
	return (sampleRate / (bpm / 60.0)) / float64(INTERVAL)
}

// intervalToBpmAt converts a beat interval in energy-envelope units back to
// a tempo for an arbitrary sample rate.
func intervalToBpmAt(interval, sampleRate float64) float64 {
	return sampleRate / (interval * float64(INTERVAL)) * 60.0
}

// autodifferenceAt is the same contrast measurement as autodifference, but
// evaluated at a caller-chosen midpoint so the analysis is deterministic.
func autodifferenceAt(nrg []float32, interval, mid float64) float64 {
	beats := [...]float64{-32, -16, -8, -4, -2, -1, 1, 2, 4, 8, 16, 32}
	nobeats := [...]float64{-0.5, -0.25, 0.25, 0.5}

	v := sample(nrg, mid)
	var diff, total float64

	for n := 0; n < len(beats)/2; n++ {
		y := sample(nrg, mid+beats[n]*interval)
		w := 1.0 / math.Abs(beats[n])
		diff += w * math.Abs(y-v)
		total += w
	}
	for n := 0; n < len(nobeats)/2; n++ {
		y := sample(nrg, mid+nobeats[n]*interval)
		w := math.Abs(nobeats[n])
		diff -= w * math.Abs(y-v)
		total += w
	}
	return diff / total
}

// windowScore averages the autodifference over deterministic, evenly spaced
// probe positions covering the whole window.
func windowScore(nrg []float32, interval float64, probes int) float64 {
	var sum float64
	for s := 0; s < probes; s++ {
		mid := (float64(s) + 0.5) * float64(len(nrg)) / float64(probes)
		sum += autodifferenceAt(nrg, interval, mid)
	}
	return sum / float64(probes)
}

// scanTempo returns the interval (energy units) with the lowest contrast
// score together with that score.
func scanTempo(nrg []float32, minBPM, maxBPM, sampleRate float64, steps, probes int) (bestInterval, bestScore float64) {
	slowest := bpmToIntervalAt(minBPM, sampleRate)
	fastest := bpmToIntervalAt(maxBPM, sampleRate)
	step := (slowest - fastest) / float64(steps)

	bestInterval = fastest
	bestScore = math.Inf(1)
	for interval := fastest; interval <= slowest; interval += step {
		score := windowScore(nrg, interval, probes)
		if score < bestScore {
			bestScore = score
			bestInterval = interval
		}
	}
	return bestInterval, bestScore
}

type windowTempo struct {
	start, end, center float64
	bpm, confidence    float64
	ambiguous          bool
	candidates         []float64
}

// analyseWindow estimates the local tempo inside one energy window.
func analyseWindow(nrg []float32, start, end float64, cfg Config) windowTempo {
	bestInterval, bestScore := scanTempo(nrg, cfg.MinBPM, cfg.MaxBPM, cfg.SampleRate, cfg.Steps, cfg.Samples)
	bestBPM := intervalToBpmAt(bestInterval, cfg.SampleRate)

	// Reference score: the average contrast over the whole range. A clear
	// tempo trough beats this reference by a wide margin; silence does not.
	var referenceSum float64
	slowest := bpmToIntervalAt(cfg.MinBPM, cfg.SampleRate)
	fastest := bpmToIntervalAt(cfg.MaxBPM, cfg.SampleRate)
	for p := 0; p < referenceProbes; p++ {
		interval := fastest + (slowest-fastest)*(float64(p)+0.5)/float64(referenceProbes)
		referenceSum += windowScore(nrg, interval, cfg.Samples)
	}
	reference := referenceSum / referenceProbes
	confidence := (reference - bestScore) / (math.Abs(reference) + 1e-12)
	confidence = math.Max(0, math.Min(1, confidence))

	candidates := []float64{bestBPM}
	ambiguous := false
	margin := ambiguityThreshold * (math.Abs(bestScore) + 1e-12)

	// Check the octave harmonics (half/double tempo) that stay in range.
	for _, altInterval := range []float64{bestInterval * 2, bestInterval / 2} {
		altBPM := intervalToBpmAt(altInterval, cfg.SampleRate)
		if altBPM < cfg.MinBPM || altBPM > cfg.MaxBPM {
			continue
		}
		altScore := windowScore(nrg, altInterval, cfg.Samples)
		if altScore <= bestScore+margin {
			ambiguous = true
		}
		candidates = append(candidates, altBPM)
	}
	sort.Float64s(candidates)

	return windowTempo{
		start:      start,
		end:        end,
		center:     (start + end) / 2,
		bpm:        bestBPM,
		confidence: confidence,
		ambiguous:  ambiguous,
		candidates: candidates,
	}
}

func medianF(xs []float64) float64 {
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

// dedupeAndSortCandidates merges per-window harmonic candidates, collapsing
// values within 2% of each other.
func dedupeAndSortCandidates(set map[float64]struct{}) []float64 {
	out := make([]float64, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Float64s(out)
	kept := out[:0]
	for _, v := range out {
		if len(kept) > 0 && math.Abs(v-kept[len(kept)-1]) <= 0.02*kept[len(kept)-1] {
			continue
		}
		kept = append(kept, v)
	}
	return kept
}

// segmentWindows groups overlapping window estimates into contiguous tempo
// segments. A new segment opens only once two consecutive windows deviate
// from the running median by more than changeThreshold, so a single noisy
// window cannot fragment the map.
func segmentWindows(wins []windowTempo, duration float64) []TempoSegment {
	if len(wins) == 0 {
		return nil
	}

	var segments []TempoSegment
	group := []int{0}
	segmentStart := 0.0

	flush := func(idxs []int, start, end float64) {
		bpms := make([]float64, len(idxs))
		var confidenceSum float64
		ambiguous := false
		candSet := map[float64]struct{}{}
		for j, i := range idxs {
			bpms[j] = wins[i].bpm
			confidenceSum += wins[i].confidence
			if wins[i].ambiguous {
				ambiguous = true
			}
			for _, c := range wins[i].candidates {
				candSet[c] = struct{}{}
			}
		}
		segments = append(segments, TempoSegment{
			Start:      start,
			End:        end,
			BPM:        medianF(bpms),
			Confidence: confidenceSum / float64(len(idxs)),
			Ambiguous:  ambiguous,
			Candidates: dedupeAndSortCandidates(candSet),
		})
	}

	for i := 1; i < len(wins); i++ {
		med := medianF(collectBpms(wins, group))
		deviates := math.Abs(wins[i].bpm-med)/med > changeThreshold
		if deviates && (i+1 >= len(wins) ||
			math.Abs(wins[i+1].bpm-med)/med > changeThreshold) {
			boundary := (wins[i-1].center + wins[i].center) / 2
			flush(group, segmentStart, boundary)
			segmentStart = boundary
			group = []int{i}
			continue
		}
		group = append(group, i)
	}
	flush(group, segmentStart, duration)

	// Cover the full timeline, including parts not centred on a window.
	segments[0].Start = 0
	segments[len(segments)-1].End = duration
	return segments
}

func collectBpms(wins []windowTempo, idxs []int) []float64 {
	out := make([]float64, len(idxs))
	for j, i := range idxs {
		out[j] = wins[i].bpm
	}
	return out
}

// placeBeats lays a phase-aligned beat grid over every segment and merges
// the grids into one strictly increasing timeline. Phase is anchored on the
// strongest onset in the segment's first window. Grid points are snapped to
// the local energy maximum so a one-frame phase rounding error cannot make
// an audible beat look silent.
func placeBeats(nrg []float32, segments []TempoSegment, cfg Config) []Beat {
	onset := make([]float64, len(nrg))
	var peakEnergy float64
	for i := 1; i < len(nrg); i++ {
		if nrg[i] > nrg[i-1] {
			onset[i] = float64(nrg[i] - nrg[i-1])
		}
		if float64(nrg[i]) > peakEnergy {
			peakEnergy = float64(nrg[i])
		}
	}
	silenceFloor := silenceFloorRatio * peakEnergy

	winN := int(cfg.Window.Seconds() * cfg.SampleRate / float64(INTERVAL))
	beats := make([]Beat, 0)

	// snapRadius is half of the smallest plausible half-beat spacing; a real
	// beat is never farther than this from its grid position.
	snapRadius := int(math.Round(
		bpmToIntervalAt(cfg.MaxBPM, cfg.SampleRate) / 2))
	if snapRadius < 1 {
		snapRadius = 1
	}

	// localMax returns the index and value of the strongest energy frame in
	// [center-r, center+r], clamped to the envelope array.
	localMax := func(center, r int) (int, float64) {
		lo, hi := center-r, center+r+1
		if lo < 0 {
			lo = 0
		}
		if hi > len(nrg) {
			hi = len(nrg)
		}
		bestIdx, bestVal := center, float64(nrg[center])
		for i := lo; i < hi; i++ {
			if v := float64(nrg[i]); v > bestVal {
				bestVal = v
				bestIdx = i
			}
		}
		return bestIdx, bestVal
	}

	for _, seg := range segments {
		interval := bpmToIntervalAt(seg.BPM, cfg.SampleRate)
		segStart := int(math.Round(seg.Start * cfg.SampleRate / float64(INTERVAL)))
		segEnd := int(math.Round(seg.End * cfg.SampleRate / float64(INTERVAL)))
		if segStart < 0 {
			segStart = 0
		}
		if segStart > len(nrg) {
			segStart = len(nrg)
		}
		if segEnd < 0 {
			segEnd = 0
		}
		if segEnd > len(nrg) {
			segEnd = len(nrg)
		}
		if segEnd <= segStart {
			continue
		}

		// Anchor on the strongest onset inside the first window of the
		// segment (or the whole segment when it is shorter), then snap it to
		// the local energy maximum.
		searchEnd := segStart + winN
		if searchEnd > segEnd {
			searchEnd = segEnd
		}
		anchor := segStart
		var strongest float64 = -1
		for i := segStart; i < searchEnd; i++ {
			if onset[i] > strongest {
				strongest = onset[i]
				anchor = i
			}
		}
		radius := snapRadius
		if int(interval/2) < radius {
			radius = int(interval / 2)
		}
		anchor, _ = localMax(anchor, radius)

		firstK := int(math.Ceil((float64(segStart) - float64(anchor)) / interval))
		lastK := int(math.Floor((float64(segEnd-1) - float64(anchor)) / interval))
		for k := firstK; k <= lastK; k++ {
			gridIdx := int(math.Round(float64(anchor) + float64(k)*interval))
			if gridIdx < segStart || gridIdx >= segEnd {
				continue
			}
			idx, energy := localMax(gridIdx, radius)
			if idx < segStart || idx >= segEnd {
				idx = gridIdx
				energy = float64(nrg[idx])
			}
			t := (float64(idx) + 0.5) * float64(INTERVAL) / cfg.SampleRate

			// Guard the segment boundary: no inversions or duplicates.
			if len(beats) > 0 && t <= beats[len(beats)-1].Time+DuplicateTolerance {
				continue
			}

			if energy <= silenceFloor {
				beats = append(beats, Beat{
					Time:       t,
					Confidence: 0.05,
					Weak:       true,
					Silent:     true,
				})
				continue
			}

			lo := idx - int(interval)
			if lo < 0 {
				lo = 0
			}
			hi := idx + int(interval)
			if hi > len(onset) {
				hi = len(onset)
			}
			var localPeak float64
			for i := lo; i < hi; i++ {
				if onset[i] > localPeak {
					localPeak = onset[i]
				}
			}
			ratio := onset[idx] / (localPeak + 1e-12)
			confidence := seg.Confidence * (0.25 + 0.75*math.Max(0, math.Min(1, ratio)))
			beats = append(beats, Beat{
				Time:       t,
				Confidence: confidence,
				Weak:       confidence < weakConfidenceThreshold,
			})
		}
	}

	sort.Slice(beats, func(i, j int) bool { return beats[i].Time < beats[j].Time })
	return beats
}

// AnalyzeBeatMap builds a variable-tempo beat map from mono PCM float32
// samples. It returns time-sorted beat positions (with per-beat confidence,
// including silence and weak beats) and a set of contiguous local-tempo
// segments.
//
// Failure modes are explicit:
//   - nil/empty input or audio shorter than MinDuration: ErrAudioTooShort
//   - NaN or infinite samples: ErrNonFiniteSample
//   - zero/negative/non-finite sample rate: ErrInvalidSampleRate
//   - any other unusable Config field: ErrInvalidConfig
func AnalyzeBeatMap(samples []float32, cfg Config) (*BeatMap, error) {
	cfg, err := cfg.validated()
	if err != nil {
		return nil, err
	}

	minSamples := int(MinDuration.Seconds() * cfg.SampleRate)
	if len(samples) < minSamples {
		return nil, fmt.Errorf("%w: got %d samples (%.2fs), need at least %s",
			ErrAudioTooShort, len(samples), float64(len(samples))/cfg.SampleRate, MinDuration)
	}
	for i, s := range samples {
		if !isFinite(float64(s)) {
			return nil, fmt.Errorf("%w: sample %d is %v", ErrNonFiniteSample, i, s)
		}
	}

	nrg := ReadFloatArray(samples)
	duration := float64(len(samples)) / cfg.SampleRate

	winN := int(cfg.Window.Seconds() * cfg.SampleRate / float64(INTERVAL))
	hopN := int(cfg.Hop.Seconds() * cfg.SampleRate / float64(INTERVAL))
	if winN < 2 || hopN < 1 {
		return nil, fmt.Errorf("%w: window %s / hop %s too short for sample rate %v", ErrInvalidConfig, cfg.Window, cfg.Hop, cfg.SampleRate)
	}

	// Window start positions; a short tail gets one final clamped window.
	type span struct{ start, end int }
	var spans []span
	for start := 0; start+winN <= len(nrg); start += hopN {
		spans = append(spans, span{start, start + winN})
	}
	if len(spans) == 0 {
		spans = append(spans, span{0, len(nrg)})
	} else if spans[len(spans)-1].end < len(nrg) {
		spans = append(spans, span{len(nrg) - winN, len(nrg)})
	}

	wins := make([]windowTempo, 0, len(spans))
	for _, sp := range spans {
		startTime := float64(sp.start) * float64(INTERVAL) / cfg.SampleRate
		endTime := float64(sp.end) * float64(INTERVAL) / cfg.SampleRate
		wins = append(wins, analyseWindow(nrg[sp.start:sp.end], startTime, endTime, cfg))
	}

	segments := segmentWindows(wins, duration)
	beats := placeBeats(nrg, segments, cfg)

	return &BeatMap{
		Duration: duration,
		Beats:    beats,
		Segments: segments,
	}, nil
}

// AnalyzeBeatMapStream drains a PCM sample channel and analyses the full
// stream with AnalyzeBeatMap. It mirrors ProgressivelyReadFloatArray for
// callers that already produce channels of float32 samples.
func AnalyzeBeatMapStream(in <-chan float32, cfg Config) (*BeatMap, error) {
	var samples []float32
	for s := range in {
		samples = append(samples, s)
	}
	return AnalyzeBeatMap(samples, cfg)
}
