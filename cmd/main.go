package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/benjojo/bpm"
)

var (
	min                 = flag.Float64("min", 120, "min BPM you are expecting")
	max                 = flag.Float64("max", 200, "max BPM you are expecting")
	progressive         = flag.Bool("progressive", false, "Print the BPM for every period")
	progressiveInterval = flag.Int("interval", 10, "How many seconds for every progressive chunk printed")

	beatmapMode = flag.Bool("beatmap", false, "Print a variable-tempo beat map (tempo segments followed by every beat) instead of a single BPM")
	jsonOutput  = flag.Bool("json", false, "With -beatmap, emit the beat map as JSON")
	rate        = flag.Float64("rate", float64(bpm.RATE), "PCM sample rate in Hz")
	window      = flag.Duration("window", 10*time.Second, "Analysis window for -beatmap (e.g. 8s)")
	hop         = flag.Duration("hop", 5*time.Second, "Advance between analysis windows for -beatmap (e.g. 2s)")
)

func main() {
	flag.Parse()

	if flag.Arg(0) == "" {
		flag.PrintDefaults()
		os.Exit(1)
	}

	path := flag.Arg(0)
	var file *os.File
	var err error
	if path == "-" {
		file = os.Stdin
	} else {
		file, err = os.Open(path)
		if err != nil {
			log.Fatalf("Unable to open file, Err: %s", err.Error())
		}
	}
	defer file.Close()

	if *beatmapMode {
		if err := runBeatMap(file); err != nil {
			log.Fatalf("beat map analysis failed: %s", err.Error())
		}
		return
	}
	if err := runClassic(file); err != nil {
		log.Fatalf("%s", err.Error())
	}
}

func readSamples(file *os.File) ([]float32, error) {
	samples := make([]float32, 0)
	for {
		var f float32
		err := binary.Read(file, binary.LittleEndian, &f)
		if err != nil {
			break
		}
		samples = append(samples, f)
	}
	return samples, nil
}

func runBeatMap(file *os.File) error {
	samples, err := readSamples(file)
	if err != nil {
		return err
	}

	cfg := bpm.DefaultConfig()
	cfg.SampleRate = *rate
	cfg.MinBPM = *min
	cfg.MaxBPM = *max
	cfg.Window = *window
	cfg.Hop = *hop

	result, err := bpm.AnalyzeBeatMap(samples, cfg)
	if err != nil {
		return err
	}

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Printf("# beat map: %.3fs, %d segment(s), %d beat(s)\n",
		result.Duration, len(result.Segments), len(result.Beats))
	fmt.Println("# segments: start-end BPM confidence ambiguous candidates")
	for _, s := range result.Segments {
		fmt.Printf("segment %.3f %.3f %.3f %.3f %t %v\n",
			s.Start, s.End, s.BPM, s.Confidence, s.Ambiguous, s.Candidates)
	}
	fmt.Println("# beats: time confidence weak silent")
	for _, b := range result.Beats {
		fmt.Printf("beat %.3f %.3f %t %t\n", b.Time, b.Confidence, b.Weak, b.Silent)
	}
	return nil
}

func runClassic(file *os.File) error {
	in := make(chan float32)
	out := make(chan float32)

	go bpm.ProgressivelyReadFloatArray(in, out)

	done := make(chan error, 1)
	go func() {
		done <- readProgressiveVars(out, *progressive, *progressiveInterval)
	}()

	for {
		var f float32
		err := binary.Read(file, binary.LittleEndian, &f)
		if err != nil {
			break
		}
		in <- f
	}
	close(in)

	return <-done
}

func calcChunkLen(second int) int {
	return (bpm.RATE / bpm.INTERVAL) * second
}

func readProgressiveVars(input chan float32, progressive bool, pint int) error {
	if progressive {
		maxsize := calcChunkLen(pint)

		nrg := make([]float32, 0)
		for f := range input {
			nrg = append(nrg, f)
			if len(nrg) == maxsize {
				fmt.Printf("%f\n", bpm.ScanForBpm(nrg, *min, *max, 1024, 1024))
				nrg = make([]float32, 0)
			}
		}
		return nil
	}

	nrg := make([]float32, 0)
	for f := range input {
		nrg = append(nrg, f)
	}
	fmt.Printf("%f\n", bpm.ScanForBpm(nrg, *min, *max, 1024, 1024))
	return nil
}
