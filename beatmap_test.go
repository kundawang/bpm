package bpm

import (
	"errors"
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Synthetic test-signal generators
// ---------------------------------------------------------------------------

// genClicks renders decaying 30ms clicks at the given times into a mono PCM
// buffer of totalSamples length. amplitude scales every click.
func genClicks(times []float64, sampleRate float64, totalSamples int, amplitude float32) []float32 {
	buf := make([]float32, totalSamples)
	clickLen := int(0.03 * sampleRate)
	if clickLen < 1 {
		clickLen = 1
	}
	for _, t := range times {
		pos := int(math.Round(t * sampleRate))
		for i := 0; i < clickLen && pos+i < len(buf); i++ {
			env := math.Exp(-6 * float64(i) / float64(clickLen))
			buf[pos+i] += amplitude * float32(env)
		}
	}
	return buf
}

// genClicksAlternating renders clicks at the given times with alternating
// strong and weak amplitudes.
func genClicksAlternating(times []float64, sampleRate float64, totalSamples int) []float32 {
	buf := make([]float32, totalSamples)
	clickLen := int(0.03 * sampleRate)
	for n, t := range times {
		amp := float32(0.9)
		if n%2 == 1 {
			amp = 0.12
		}
		pos := int(math.Round(t * sampleRate))
		for i := 0; i < clickLen && pos+i < len(buf); i++ {
			env := math.Exp(-6 * float64(i) / float64(clickLen))
			buf[pos+i] += amp * float32(env)
		}
	}
	return buf
}

func constantClickTimes(bpm float64, duration float64) []float64 {
	var times []float64
	period := 60.0 / bpm
	for t := 0.0; t < duration; t += period {
		times = append(times, t)
	}
	return times
}

// twoTempoClickTimes emits bpm1 clicks up to boundary and bpm2 afterwards,
// re-anchoring the grid at the boundary so there is no overlapping click.
func twoTempoClickTimes(bpm1, bpm2, boundary, duration float64) []float64 {
	var times []float64
	t := 0.0
	times = append(times, t)
	for t < duration {
		bpm := bpm1
		if t >= boundary {
			bpm = bpm2
		}
		next := t + 60.0/bpm
		if next >= duration {
			break
		}
		t = next
		times = append(times, t)
	}
	return times
}

// rampClickTimes emits clicks whose tempo moves linearly from bpmStart to
// bpmEnd over the whole duration.
func rampClickTimes(bpmStart, bpmEnd, duration float64) []float64 {
	var times []float64
	t := 0.0
	times = append(times, t)
	for {
		frac := t / duration
		bpm := bpmStart + (bpmEnd-bpmStart)*frac
		next := t + 60.0/bpm
		if next >= duration {
			break
		}
		t = next
		times = append(times, t)
	}
	return times
}

func nearestBeatGap(beats []Beat, t float64) float64 {
	best := math.Inf(1)
	for _, b := range beats {
		if d := math.Abs(b.Time - t); d < best {
			best = d
		}
	}
	return best
}

// coveredFraction returns the share of expected click times that have a
// detected beat within tol seconds.
func coveredFraction(beats []Beat, expected []float64, tol float64) float64 {
	hit := 0
	for _, t := range expected {
		if nearestBeatGap(beats, t) <= tol {
			hit++
		}
	}
	return float64(hit) / float64(len(expected))
}

func testConfig(sampleRate float64) Config {
	cfg := DefaultConfig()
	cfg.SampleRate = sampleRate
	cfg.MinBPM = 60
	cfg.MaxBPM = 200
	cfg.Window = 6 * time.Second
	cfg.Hop = 2 * time.Second
	return cfg
}

// ---------------------------------------------------------------------------
// Original single-value entry points still work
// ---------------------------------------------------------------------------

func TestScanForBpmStillWorks(t *testing.T) {
	const sr = 44100.0
	times := constantClickTimes(120, 12)
	samples := genClicks(times, sr, int(12*sr), 0.9)
	nrg := ReadFloatArray(samples)

	// The legacy entry keeps its random-mid behaviour; a clear click track
	// must land close to the true tempo on every run.
	for attempt := 0; attempt < 5; attempt++ {
		got := ScanForBpm(nrg, 60, 200, 1024, 256)
		if math.Abs(got-120) > 8 {
			t.Fatalf("legacy ScanForBpm attempt %d: got %.2f BPM, want ~120", attempt, got)
		}
	}
}

func TestReadFloatArrayAndProgressive(t *testing.T) {
	const sr = 44100.0
	samples := genClicks(constantClickTimes(100, 5), sr, int(5*sr), 0.9)

	slice := ReadFloatArray(samples)
	wantLen := len(samples) / INTERVAL
	if len(slice) != wantLen {
		t.Fatalf("ReadFloatArray length = %d, want %d", len(slice), wantLen)
	}

	in := make(chan float32)
	out := make(chan float32)
	go ProgressivelyReadFloatArray(in, out)
	go func() {
		for _, s := range samples {
			in <- s
		}
		close(in)
	}()
	var streamed []float32
	for v := range out {
		streamed = append(streamed, v)
	}
	if len(streamed) != len(slice) {
		t.Fatalf("progressive length = %d, want %d", len(streamed), len(slice))
	}
	for i := range slice {
		if streamed[i] != slice[i] {
			t.Fatalf("progressive output diverges at %d: %v vs %v", i, streamed[i], slice[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Variable-tempo beat map
// ---------------------------------------------------------------------------

func TestBeatMapTwoTempos(t *testing.T) {
	const sr = 22050.0
	const boundary = 16.0
	const duration = 32.0
	times := twoTempoClickTimes(90, 130, boundary, duration)
	samples := genClicks(times, sr, int(duration*sr), 0.9)

	result, err := AnalyzeBeatMap(samples, testConfig(sr))
	if err != nil {
		t.Fatalf("AnalyzeBeatMap failed: %v", err)
	}
	if len(result.Segments) < 2 {
		t.Fatalf("want at least 2 tempo segments, got %d: %+v", len(result.Segments), result.Segments)
	}
	if len(result.Beats) == 0 {
		t.Fatal("no beats produced")
	}

	// Segments must be contiguous, ordered and cover the whole timeline.
	if result.Segments[0].Start != 0 || result.Segments[len(result.Segments)-1].End != result.Duration {
		t.Fatalf("segments do not cover [0, %.2f]: %+v", result.Duration, result.Segments)
	}
	for i := 1; i < len(result.Segments); i++ {
		if result.Segments[i].Start != result.Segments[i-1].End {
			t.Fatalf("segment gap/inversion at %d: %+v", i, result.Segments)
		}
	}

	// Tempo of the material well inside each half.
	segAt := func(at float64) TempoSegment {
		for _, s := range result.Segments {
			if at >= s.Start && at < s.End {
				return s
			}
		}
		return result.Segments[len(result.Segments)-1]
	}
	first := segAt(5)
	last := segAt(duration - 5)
	if math.Abs(first.BPM-90) > 0.08*90 {
		t.Fatalf("first-half tempo = %.2f, want ~90", first.BPM)
	}
	if math.Abs(last.BPM-130) > 0.08*130 {
		t.Fatalf("second-half tempo = %.2f, want ~130", last.BPM)
	}

	// A segment boundary must sit near the actual transition.
	foundBoundary := false
	for _, s := range result.Segments {
		if math.Abs(s.Start-boundary) <= 4.0 && s.Start > 0 {
			foundBoundary = true
		}
	}
	if !foundBoundary {
		t.Fatalf("no segment boundary near %vs: %+v", boundary, result.Segments)
	}

	// Beat grid follows the clicks in both halves (transition region
	// excluded).
	var earlyClicks, lateClicks []float64
	for _, ct := range times {
		if ct < boundary-2 {
			earlyClicks = append(earlyClicks, ct)
		} else if ct > boundary+2 {
			lateClicks = append(lateClicks, ct)
		}
	}
	if c := coveredFraction(result.Beats, earlyClicks, 0.15*60/90); c < 0.8 {
		t.Fatalf("early-half beat coverage %.2f", c)
	}
	if c := coveredFraction(result.Beats, lateClicks, 0.15*60/130); c < 0.8 {
		t.Fatalf("late-half beat coverage %.2f", c)
	}

	// Beats are strictly time ordered with no inversion/duplicate at the
	// transition.
	for i := 1; i < len(result.Beats); i++ {
		gap := result.Beats[i].Time - result.Beats[i-1].Time
		if gap <= 0 {
			t.Fatalf("beat inversion at %d", i)
		}
		if gap <= DuplicateTolerance {
			t.Fatalf("duplicate beats at %d (gap %.4f)", i, gap)
		}
	}
}

func TestBeatMapConstantTempoSingleSegment(t *testing.T) {
	const sr = 32000.0
	const duration = 15.0
	samples := genClicks(constantClickTimes(120, duration), sr, int(duration*sr), 0.9)

	cfg := testConfig(sr)
	result, err := AnalyzeBeatMap(samples, cfg)
	if err != nil {
		t.Fatalf("AnalyzeBeatMap failed: %v", err)
	}
	if len(result.Segments) != 1 {
		t.Fatalf("want 1 segment for constant tempo, got %d: %+v", len(result.Segments), result.Segments)
	}
	if math.Abs(result.Segments[0].BPM-120) > 0.06*120 {
		t.Fatalf("tempo = %.2f, want ~120", result.Segments[0].BPM)
	}
	expected := constantClickTimes(120, duration)
	if c := coveredFraction(result.Beats, expected, 0.15*60/120); c < 0.85 {
		t.Fatalf("beat coverage %.2f", c)
	}
}

func TestBeatMapGradualRamp(t *testing.T) {
	const sr = 22050.0
	const duration = 24.0
	times := rampClickTimes(100, 140, duration)
	samples := genClicks(times, sr, int(duration*sr), 0.9)

	cfg := testConfig(sr)
	result, err := AnalyzeBeatMap(samples, cfg)
	if err != nil {
		t.Fatalf("AnalyzeBeatMap failed: %v", err)
	}
	if len(result.Segments) < 2 {
		t.Fatalf("ramp should produce multiple segments, got %d", len(result.Segments))
	}

	// Segments may not go backwards beyond a small noise margin.
	for i := 1; i < len(result.Segments); i++ {
		if result.Segments[i].BPM < result.Segments[i-1].BPM-2 {
			t.Fatalf("tempo inversion in ramp at %d: %.2f -> %.2f",
				i, result.Segments[i-1].BPM, result.Segments[i].BPM)
		}
	}
	first, last := result.Segments[0], result.Segments[len(result.Segments)-1]
	if math.Abs(first.BPM-100) > 0.18*100 {
		t.Fatalf("ramp start tempo %.2f, want ~100", first.BPM)
	}
	if math.Abs(last.BPM-140) > 0.18*140 {
		t.Fatalf("ramp end tempo %.2f, want ~140", last.BPM)
	}

	// No beat inversions anywhere, and the moving grid keeps tracking clicks.
	for i := 1; i < len(result.Beats); i++ {
		if result.Beats[i].Time <= result.Beats[i-1].Time {
			t.Fatalf("beat inversion at %d", i)
		}
	}
	if c := coveredFraction(result.Beats, times, 0.2*60/100); c < 0.75 {
		t.Fatalf("ramp beat coverage %.2f", c)
	}
}

// ---------------------------------------------------------------------------
// Silence, weak beats and confidence
// ---------------------------------------------------------------------------

func TestBeatMapSilence(t *testing.T) {
	const sr = 44100.0
	samples := make([]float32, int(6*sr)) // pure silence, above MinDuration

	result, err := AnalyzeBeatMap(samples, testConfig(sr))
	if err != nil {
		t.Fatalf("silence should not error: %v", err)
	}
	if len(result.Segments) == 0 || len(result.Beats) == 0 {
		t.Fatal("silence still needs a grid, got no segments/beats")
	}
	if result.Segments[0].Confidence >= 0.1 {
		t.Fatalf("silent segment confidence %.3f should be ~0", result.Segments[0].Confidence)
	}
	for _, b := range result.Beats {
		if !b.Silent || !b.Weak {
			t.Fatalf("beat at %.3f not marked silent+weak: %+v", b.Time, b)
		}
		if b.Confidence <= 0 || b.Confidence > 0.2 {
			t.Fatalf("silent beat confidence %.3f out of (0,0.2]", b.Confidence)
		}
	}
}

func TestBeatMapTransitionAcrossSilence(t *testing.T) {
	const sr = 22050.0
	const duration = 28.0
	buf := make([]float32, int(duration*sr))
	clickLen := int(math.Round(0.03 * sr))

	putClick := func(at float64) {
		pos := int(math.Round(at * sr))
		for i := 0; i < clickLen && pos+i < len(buf); i++ {
			env := math.Exp(-6 * float64(i) / float64(clickLen))
			buf[pos+i] += 0.9 * float32(env)
		}
	}

	// 90 BPM clicks for the first 10s, silence from 10s to 18s, then
	// 130 BPM clicks for the rest: a tempo change hidden inside a gap.
	for _, at := range constantClickTimes(90, 10) {
		putClick(at)
	}
	for _, at := range constantClickTimes(130, 10) {
		putClick(18 + at)
	}

	result, err := AnalyzeBeatMap(buf, testConfig(sr))
	if err != nil {
		t.Fatalf("AnalyzeBeatMap failed: %v", err)
	}

	// Strictly increasing timeline, no duplicates, no inversions — including
	// across the silent gap and the tempo change.
	for i := 1; i < len(result.Beats); i++ {
		gap := result.Beats[i].Time - result.Beats[i-1].Time
		if gap <= 0 {
			t.Fatalf("beat inversion at %d (%.3f -> %.3f)",
				i, result.Beats[i-1].Time, result.Beats[i].Time)
		}
		if gap <= DuplicateTolerance {
			t.Fatalf("duplicate beat at %d gap %.4f", i, gap)
		}
	}

	// The silent middle must still carry a (low-confidence) grid so clip
	// editing has something to snap to.
	var silentInGap int
	for _, b := range result.Beats {
		if b.Time > 10 && b.Time < 18 && b.Silent {
			silentInGap++
		}
	}
	if silentInGap == 0 {
		t.Fatal("no silent grid beats maintained across the 10-18s gap")
	}

	// Tempo recovers on the far side of the silence.
	segAt := func(at float64) TempoSegment {
		for _, s := range result.Segments {
			if at >= s.Start && at < s.End {
				return s
			}
		}
		return result.Segments[len(result.Segments)-1]
	}
	if math.Abs(segAt(5).BPM-90) > 0.12*90 {
		t.Fatalf("pre-gap tempo %.2f, want ~90", segAt(5).BPM)
	}
	if math.Abs(segAt(duration-3).BPM-130) > 0.12*130 {
		t.Fatalf("post-gap tempo %.2f, want ~130", segAt(duration-3).BPM)
	}

	// Real clicks on both sides are still matched by beats.
	var early, late []float64
	for _, at := range constantClickTimes(90, 10) {
		early = append(early, at)
	}
	for _, at := range constantClickTimes(130, 10) {
		late = append(late, 18+at)
	}
	if c := coveredFraction(result.Beats, early, 0.15*60/90); c < 0.8 {
		t.Fatalf("pre-gap coverage %.2f", c)
	}
	if c := coveredFraction(result.Beats, late, 0.15*60/130); c < 0.8 {
		t.Fatalf("post-gap coverage %.2f", c)
	}
}

func TestBeatMapWeakVersusStrongBeats(t *testing.T) {
	const sr = 22050.0
	const duration = 12.0
	times := constantClickTimes(100, duration)
	samples := genClicksAlternating(times, sr, int(duration*sr))

	result, err := AnalyzeBeatMap(samples, testConfig(sr))
	if err != nil {
		t.Fatalf("AnalyzeBeatMap failed: %v", err)
	}
	var weak, strong int
	for _, b := range result.Beats {
		if b.Weak {
			weak++
		} else {
			strong++
		}
		if b.Confidence <= 0 || b.Confidence > 1 {
			t.Fatalf("confidence %.3f out of (0,1]", b.Confidence)
		}
	}
	if weak == 0 || strong == 0 {
		t.Fatalf("want both weak and strong beats, got weak=%d strong=%d", weak, strong)
	}
	if c := coveredFraction(result.Beats, times, 0.15*60/100); c < 0.8 {
		t.Fatalf("coverage %.2f with alternating accents", c)
	}
}

// ---------------------------------------------------------------------------
// Explicit failure modes
// ---------------------------------------------------------------------------

func TestShortAudioRejected(t *testing.T) {
	const sr = 44100.0
	cfg := testConfig(sr)

	for _, n := range []int{0, int(0.5 * sr), int(1.99 * sr)} {
		_, err := AnalyzeBeatMap(make([]float32, n), cfg)
		if !errors.Is(err, ErrAudioTooShort) {
			t.Fatalf("n=%d: want ErrAudioTooShort, got %v", n, err)
		}
	}

	// Exactly MinDuration is accepted.
	_, err := AnalyzeBeatMap(make([]float32, int(MinDuration.Seconds()*sr)), cfg)
	if err != nil {
		t.Fatalf("audio at MinDuration should be accepted: %v", err)
	}
}

func TestNonFiniteSamplesRejected(t *testing.T) {
	const sr = 22050.0
	cfg := testConfig(sr)

	good := genClicks(constantClickTimes(120, 4), sr, int(4*sr), 0.9)
	bad1 := append(append([]float32(nil), good...), float32(math.NaN()))
	if _, err := AnalyzeBeatMap(bad1, cfg); !errors.Is(err, ErrNonFiniteSample) {
		t.Fatalf("NaN: want ErrNonFiniteSample, got %v", err)
	}

	bad2 := append(append([]float32(nil), good...), float32(math.Inf(1)))
	if _, err := AnalyzeBeatMap(bad2, cfg); !errors.Is(err, ErrNonFiniteSample) {
		t.Fatalf("+Inf: want ErrNonFiniteSample, got %v", err)
	}
}

func TestInvalidSampleRateRejected(t *testing.T) {
	samples := make([]float32, 44100*3)
	cfg := DefaultConfig()

	cfg.SampleRate = -44100
	if _, err := AnalyzeBeatMap(samples, cfg); !errors.Is(err, ErrInvalidSampleRate) {
		t.Fatalf("negative rate: want ErrInvalidSampleRate, got %v", err)
	}
	cfg.SampleRate = math.NaN()
	if _, err := AnalyzeBeatMap(samples, cfg); !errors.Is(err, ErrInvalidSampleRate) {
		t.Fatalf("NaN rate: want ErrInvalidSampleRate, got %v", err)
	}
	cfg.SampleRate = math.Inf(-1)
	if _, err := AnalyzeBeatMap(samples, cfg); !errors.Is(err, ErrInvalidSampleRate) {
		t.Fatalf("-Inf rate: want ErrInvalidSampleRate, got %v", err)
	}

	// Zero is treated as "unset" and defaulted, not rejected.
	cfg = DefaultConfig()
	cfg.SampleRate = 0
	if _, err := AnalyzeBeatMap(samples, cfg); err != nil {
		t.Fatalf("zero rate should default, got error: %v", err)
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	samples := make([]float32, 44100*4)
	base := DefaultConfig()

	cfg := base
	cfg.MinBPM, cfg.MaxBPM = 200, 100
	if _, err := AnalyzeBeatMap(samples, cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("inverted range: want ErrInvalidConfig, got %v", err)
	}

	cfg = base
	cfg.MinBPM = cfg.MaxBPM
	if _, err := AnalyzeBeatMap(samples, cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty range: want ErrInvalidConfig, got %v", err)
	}

	cfg = base
	cfg.Window = 500 * time.Millisecond
	if _, err := AnalyzeBeatMap(samples, cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("tiny window: want ErrInvalidConfig, got %v", err)
	}

	cfg = base
	cfg.Hop = cfg.Window * 2
	if _, err := AnalyzeBeatMap(samples, cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("hop>window: want ErrInvalidConfig, got %v", err)
	}

	cfg = base
	cfg.MinBPM = math.NaN()
	if _, err := AnalyzeBeatMap(samples, cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NaN bound: want ErrInvalidConfig, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Double/half tempo ambiguity
// ---------------------------------------------------------------------------

func TestOctaveAmbiguityReported(t *testing.T) {
	const sr = 22050.0
	const duration = 14.0
	samples := genClicks(constantClickTimes(120, duration), sr, int(duration*sr), 0.9)

	// Wide range: both 60 and 240 harmonics are reachable, so they must be
	// surfaced as candidates alongside 120.
	cfg := testConfig(sr)
	cfg.MinBPM = 60
	cfg.MaxBPM = 250
	wide, err := AnalyzeBeatMap(samples, cfg)
	if err != nil {
		t.Fatalf("wide-range analysis failed: %v", err)
	}
	seg := wide.Segments[0]
	if math.Abs(seg.BPM-120) > 0.1*120 {
		t.Fatalf("wide-range tempo %.2f, want ~120", seg.BPM)
	}
	hasNear := func(target float64) bool {
		for _, c := range seg.Candidates {
			if math.Abs(c-target) <= 0.05*target {
				return true
			}
		}
		return false
	}
	if !hasNear(60) || !hasNear(120) || !hasNear(240) {
		t.Fatalf("candidates %v should include ~60, ~120 and ~240", seg.Candidates)
	}

	// Restricted range excludes the harmonics: no ambiguity, single candidate.
	cfg.MinBPM = 100
	cfg.MaxBPM = 160
	narrow, err := AnalyzeBeatMap(samples, cfg)
	if err != nil {
		t.Fatalf("narrow-range analysis failed: %v", err)
	}
	nseg := narrow.Segments[0]
	if nseg.Ambiguous {
		t.Fatalf("restricted range must not flag ambiguity: %+v", nseg)
	}
	if len(nseg.Candidates) != 1 || math.Abs(nseg.Candidates[0]-120) > 0.05*120 {
		t.Fatalf("restricted candidates %v, want only ~120", nseg.Candidates)
	}
}

// ---------------------------------------------------------------------------
// Streaming wrapper
// ---------------------------------------------------------------------------

func TestAnalyzeBeatMapStreamMatchesSlice(t *testing.T) {
	const sr = 22050.0
	const duration = 16.0
	samples := genClicks(twoTempoClickTimes(90, 130, 8, duration),
		sr, int(duration*sr), 0.9)
	cfg := testConfig(sr)

	fromSlice, err := AnalyzeBeatMap(samples, cfg)
	if err != nil {
		t.Fatalf("slice analysis failed: %v", err)
	}

	ch := make(chan float32, 1024)
	go func() {
		for _, s := range samples {
			ch <- s
		}
		close(ch)
	}()
	fromStream, err := AnalyzeBeatMapStream(ch, cfg)
	if err != nil {
		t.Fatalf("stream analysis failed: %v", err)
	}
	if len(fromStream.Beats) != len(fromSlice.Beats) ||
		len(fromStream.Segments) != len(fromSlice.Segments) {
		t.Fatalf("stream result shape diverges: beats %d vs %d, segments %d vs %d",
			len(fromStream.Beats), len(fromSlice.Beats),
			len(fromStream.Segments), len(fromSlice.Segments))
	}
	for i := range fromSlice.Beats {
		if fromStream.Beats[i].Time != fromSlice.Beats[i].Time {
			t.Fatalf("beat %d diverges: %.4f vs %.4f",
				i, fromStream.Beats[i].Time, fromSlice.Beats[i].Time)
		}
	}
}
