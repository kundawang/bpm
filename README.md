# bpm
[![GoDoc](https://godoc.org/github.com/benjojo/bpm?status.svg)](https://godoc.org/github.com/benjojo/bpm)


Library and tool for dealing with beats per second detection in Go

This is a direct port of Mark Hills [bpm-tools](http://www.pogo.org.uk/~mark/bpm-tools/).

For that reason, it is also under the same licence as that utility, ( GPLv2 ).

You can use this version as a libary too, In testing you may find it produces slightly
different results than bpm-tools. This is because both tools use a random float on mid
point selection, Ideally it would use a static one, but I won't replicate drand exactly
to just get 1:1 matching with the offical tools.

## Usage of the command line util

You need to feed the command line utility PCM 32 bit little edian floats (mono), there are
two easy ways to do this:

Sox:

```
sox "$FILE" -r 44100 -e float -c 1 -t raw - | ./cmd /dev/stdin
```

ffmpeg:

```
ffmpeg -v quiet -i 1479012090.ts -f f32le -ac 1 -c:a pcm_f32le -ar 44100 pipe:1 | ./cmd /dev/stdin
```

You can also ask for a "snapshot" per second of the BPM calculated, using progressive mode:

```
$ ffmpeg -v quiet -i 1479012090.ts -f f32le -ac 1 -c:a pcm_f32le -ar 44100 pipe:1 | ./cmd -progressive=true /dev/stdin
179.020979
193.086109
191.044776
```

The first value is the BPM for the first 10 seconds, the 2nd for 10-20 seconds, 3rd is the 20-30th (and more until you stop
giving it data)

## Variable-tempo beat maps

A single average BPM cannot describe a track that changes tempo (for example
a 90 BPM intro that jumps to 130 BPM after the breakdown). Use
`AnalyzeBeatMap` (or the `-beatmap` CLI flag) to get a time-sorted list of
beat positions plus contiguous local-tempo segments:

```go
cfg := bpm.DefaultConfig()           // 44.1kHz, 60-200 BPM, 10s windows / 5s hop
cfg.MinBPM = 70                      // restrict the tempo range
cfg.MaxBPM = 180
cfg.Window = 8 * time.Second         // analysis window length
cfg.Hop = 2 * time.Second            // advance between windows

mapResult, err := bpm.AnalyzeBeatMap(pcmSamples, cfg)
// mapResult.Segments: [{Start, End, BPM, Confidence, Ambiguous, Candidates}, ...]
// mapResult.Beats:    [{Time, Confidence, Weak, Silent}, ...]
```

Properties of the result:

* `Beats` are strictly time ordered with no duplicates or inversions,
  including across tempo changes. Beats closer than
  `bpm.DuplicateTolerance` (80ms) at a segment boundary are merged.
* Silent and weak beats keep a small non-zero confidence and are flagged
  with `Silent` / `Weak`, so the grid continues through quiet passages
  instead of developing gaps.
* When a double- or half-time tempo scores nearly as well as the best
  estimate, the segment is marked `Ambiguous` and every in-range harmonic
  is listed in `Candidates`.
* A streaming variant `AnalyzeBeatMapStream` accepts a sample channel.

Explicit failure modes (use `errors.Is`):

* audio shorter than `bpm.MinDuration` (2s) — `ErrAudioTooShort`
* NaN or infinite PCM samples — `ErrNonFiniteSample`
* zero/negative/non-finite sample rate — `ErrInvalidSampleRate`
* bad tempo range, window or hop — `ErrInvalidConfig`

The CLI exposes the same analysis; add `-json` for machine-readable output
and `-` to read from stdin:

```
$ ffmpeg -v quiet -i track.ts -f f32le -ac 1 -c:a pcm_f32le -ar 44100 pipe:1 \
    | ./cmd -beatmap -min 70 -max 180 -window 8s -hop 2s -
# beat map: 214.000s, 2 segment(s), 397 beat(s)
# segments: start-end BPM confidence ambiguous candidates
segment 0.000 96.500 90.128 1.000 false [90.035 180.070]
segment 96.500 214.000 129.963 0.968 false [64.933 129.867]
# beats: time confidence weak silent
beat 0.009 0.950 false false
...
```

