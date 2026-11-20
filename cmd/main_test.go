package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjojo/bpm"
)

const cmdTestSR = 22050.0

func writePCM(t *testing.T, samples []float32) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "in.f32le")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf [4]byte
	for _, s := range samples {
		bits := math.Float32bits(s)
		buf[0] = byte(bits)
		buf[1] = byte(bits >> 8)
		buf[2] = byte(bits >> 16)
		buf[3] = byte(bits >> 24)
		if _, err := f.Write(buf[:]); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func clickTimes(bpm float64, duration float64) []float64 {
	var times []float64
	period := 60.0 / bpm
	for at := 0.0; at < duration; at += period {
		times = append(times, at)
	}
	return times
}

func synthClicks(t *testing.T, times []float64, duration float64) ([]float32, string) {
	t.Helper()
	n := int(duration * cmdTestSR)
	buf := make([]float32, n)
	clickLen := int(math.Round(0.03 * cmdTestSR))
	for _, at := range times {
		pos := int(math.Round(at * cmdTestSR))
		for i := 0; i < clickLen && pos+i < n; i++ {
			env := math.Exp(-6 * float64(i) / float64(clickLen))
			buf[pos+i] += 0.9 * float32(env)
		}
	}
	return buf, writePCM(t, buf)
}

// captureOutput swaps os.Stdout for a pipe and runs fn.
func captureOutput(t *testing.T, fn func() error) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	runErr := fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = orig
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("run failed: %v", runErr)
	}
	return string(out)
}

func setFlags(t *testing.T, fn func()) {
	t.Helper()
	oldMin, oldMax := *min, *max
	oldBM, oldJSON := *beatmapMode, *jsonOutput
	oldRate, oldWindow, oldHop := *rate, *window, *hop
	oldProg, oldInterval := *progressive, *progressiveInterval
	fn()
	t.Cleanup(func() {
		*min, *max = oldMin, oldMax
		*beatmapMode, *jsonOutput = oldBM, oldJSON
		*rate, *window, *hop = oldRate, oldWindow, oldHop
		*progressive, *progressiveInterval = oldProg, oldInterval
	})
}

func TestCLIClassicSingleValue(t *testing.T) {
	// The legacy entry hardcodes 44.1kHz in its interval math, so the test
	// fixture must be rendered at that rate.
	const classicSR = 44100.0
	duration := 12.0
	n := int(duration * classicSR)
	buf := make([]float32, n)
	clickLen := int(math.Round(0.03 * classicSR))
	for _, at := range clickTimes(120, duration) {
		pos := int(math.Round(at * classicSR))
		for i := 0; i < clickLen && pos+i < n; i++ {
			env := math.Exp(-6 * float64(i) / float64(clickLen))
			buf[pos+i] += 0.9 * float32(env)
		}
	}
	path := writePCM(t, buf)

	setFlags(t, func() {
		*min = 60
		*max = 200
		*beatmapMode = false
	})

	out := captureOutput(t, func() error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return runClassic(f)
	})
	out = strings.TrimSpace(out)
	var got float64
	if _, err := fmt.Sscan(out, &got); err != nil {
		t.Fatalf("classic output %q not a single float: %v", out, err)
	}
	if math.Abs(got-120) > 10 {
		t.Fatalf("classic BPM %.2f, want ~120", got)
	}
}

func TestCLIBeatMapText(t *testing.T) {
	var times []float64
	for _, at := range clickTimes(90, 16) {
		times = append(times, at)
	}
	for _, at := range clickTimes(130, 16) {
		if at == 0 {
			continue
		}
		times = append(times, 16+at)
	}
	_, path := synthClicks(t, times, 32)

	setFlags(t, func() {
		*min = 60
		*max = 200
		*beatmapMode = true
		*jsonOutput = false
		*rate = cmdTestSR
		*window = 6 * time.Second
		*hop = 2 * time.Second
	})

	out := captureOutput(t, func() error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return runBeatMap(f)
	})

	if !strings.Contains(out, "# beat map") ||
		!strings.Contains(out, "segment ") ||
		!strings.Contains(out, "beat ") {
		t.Fatalf("beat map text output missing lines:\n%s", out)
	}
	segLines := strings.Count(out, "\nsegment ")
	beatLines := strings.Count(out, "\nbeat ")
	if segLines < 2 {
		t.Fatalf("want >=2 segment lines, got %d:\n%s", segLines, out)
	}
	if beatLines == 0 {
		t.Fatalf("no beat lines:\n%s", out)
	}
}

func TestCLIBeatMapJSON(t *testing.T) {
	_, path := synthClicks(t, clickTimes(120, 14), 14)

	setFlags(t, func() {
		*min = 60
		*max = 200
		*beatmapMode = true
		*jsonOutput = true
		*rate = cmdTestSR
		*window = 6 * time.Second
		*hop = 2 * time.Second
	})

	out := captureOutput(t, func() error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return runBeatMap(f)
	})

	var parsed bpm.BeatMap
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(parsed.Beats) == 0 || len(parsed.Segments) == 0 {
		t.Fatalf("parsed empty beat map: %+v", parsed)
	}
	if math.Abs(parsed.Segments[0].BPM-120) > 0.1*120 {
		t.Fatalf("JSON tempo %.2f, want ~120", parsed.Segments[0].BPM)
	}
}

func TestCLIBeatMapRejectsShortInput(t *testing.T) {
	_, path := synthClicks(t, clickTimes(120, 1), 1)

	setFlags(t, func() {
		*beatmapMode = true
		*rate = cmdTestSR
	})

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := runBeatMap(f); err == nil {
		t.Fatal("short input should fail with ErrAudioTooShort")
	}
}
