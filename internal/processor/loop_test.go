package processor

import (
	"errors"
	"math"
	"testing"
	"time"
)

const testRate = 8000

// sineClip builds a mono or stereo clip from a sine wave. Channel 1 is phase
// shifted so a bug that mixes channels becomes visible.
func sineClip(t *testing.T, frames, channels int, cycles float64) PCMClip {
	t.Helper()

	clip := PCMClip{
		SampleRate: testRate,
		Channels:   make([][]float32, channels),
	}

	for channel := 0; channel < channels; channel++ {
		clip.Channels[channel] = make([]float32, frames)

		phase := 0.0
		if channel == 1 {
			phase = math.Pi / 3
		}

		for frame := 0; frame < frames; frame++ {
			position := float64(frame) / float64(frames)
			clip.Channels[channel][frame] = float32(
				0.8 * math.Sin(2*math.Pi*cycles*position+phase),
			)
		}
	}

	return clip
}

func stepClip(frames, channels int) PCMClip {
	clip := PCMClip{SampleRate: testRate, Channels: make([][]float32, channels)}

	for channel := range clip.Channels {
		clip.Channels[channel] = make([]float32, frames)

		for frame := range clip.Channels[channel] {
			if frame < frames/2 {
				clip.Channels[channel][frame] = -1
			} else {
				clip.Channels[channel][frame] = 1
			}
		}
	}

	return clip
}

func TestPlanLoopFormula(t *testing.T) {
	tests := []struct {
		name       string
		clip       int
		loops      int
		crossfade  int
		wantUnit   int
		wantFrames int
	}{
		{name: "no crossfade", clip: 1000, loops: 3, crossfade: 0, wantUnit: 1000, wantFrames: 3000},
		{name: "one loop", clip: 1000, loops: 1, crossfade: 100, wantUnit: 900, wantFrames: 900},
		{name: "three loops", clip: 1000, loops: 3, crossfade: 100, wantUnit: 900, wantFrames: 2700},
		{name: "half clip fade", clip: 1000, loops: 2, crossfade: 500, wantUnit: 500, wantFrames: 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timing, err := PlanLoop(tt.clip, testRate, tt.loops, tt.crossfade)
			if err != nil {
				t.Fatalf("PlanLoop returned error: %v", err)
			}

			if timing.UnitFrames != tt.wantUnit {
				t.Fatalf("unit frames = %d, want %d", timing.UnitFrames, tt.wantUnit)
			}

			if timing.OutputFrames != tt.wantFrames {
				t.Fatalf("output frames = %d, want %d", timing.OutputFrames, tt.wantFrames)
			}

			want := tt.loops * (tt.clip - tt.crossfade)
			if timing.OutputFrames != want {
				t.Fatalf("output frames = %d, formula N*(C-F) = %d", timing.OutputFrames, want)
			}
		})
	}
}

func TestPlanLoopRejectsCrossfadeLongerThanHalfClip(t *testing.T) {
	if _, err := PlanLoop(1000, testRate, 2, 501); !errors.Is(err, ErrCrossfadeTooLong) {
		t.Fatalf("expected ErrCrossfadeTooLong, got %v", err)
	}

	if _, err := PlanLoop(10, testRate, 2, 10); !errors.Is(err, ErrCrossfadeTooLong) {
		t.Fatalf("expected ErrCrossfadeTooLong for full-length fade, got %v", err)
	}
}

func TestLoopCountForTarget(t *testing.T) {
	tests := []struct {
		unit    int
		target  int
		want    int
		name    string
		special string
	}{
		{name: "exact", unit: 100, target: 300, want: 3},
		{name: "round up", unit: 100, target: 301, want: 4},
		{name: "below one unit", unit: 100, target: 10, want: 1},
		{name: "target zero", unit: 100, target: 0, want: 1},
		{name: "invalid unit", unit: 0, target: 100, want: 0, special: "zero unit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LoopCountForTarget(tt.unit, tt.target); got != tt.want {
				t.Fatalf("LoopCountForTarget(%d, %d) = %d, want %d", tt.unit, tt.target, got, tt.want)
			}
		})
	}
}

func TestPlanLoopForDurationCoversRequestedDuration(t *testing.T) {
	const clipFrames = 4000 // 0.5s at 8kHz

	// 0.25s crossfade = 2000 frames, exactly half the clip.
	timing, err := PlanLoopForDuration(clipFrames, testRate, 2e9, 250e6, 0)
	if err != nil {
		t.Fatalf("PlanLoopForDuration returned error: %v", err)
	}

	if timing.CrossfadeFrames != 2000 {
		t.Fatalf("crossfade frames = %d, want 2000", timing.CrossfadeFrames)
	}

	// Unit P = C-F = 2000 frames, 2s target = 16000 frames -> 8 units.
	if timing.LoopCount != 8 {
		t.Fatalf("loop count = %d, want 8", timing.LoopCount)
	}

	if timing.OutputFrames != timing.LoopCount*timing.UnitFrames {
		t.Fatalf(
			"output frames = %d, want N*P = %d",
			timing.OutputFrames,
			timing.LoopCount*timing.UnitFrames,
		)
	}

	if got, want := timing.OutputFrames, 2*testRate; got != want {
		t.Fatalf("output frames = %d, want %d", got, want)
	}
}

func TestPlanLoopForDurationRoundsUpToWholeUnits(t *testing.T) {
	const clipFrames = 4000

	// 1.1s at 8kHz = 8800 frames; unit is 2000 frames -> 5 units = 10000.
	// The output is a whole number of units, which keeps replaying the
	// encoded file seamless.
	timing, err := PlanLoopForDuration(clipFrames, testRate, 1_100_000_000, 250e6, 0)
	if err != nil {
		t.Fatalf("PlanLoopForDuration returned error: %v", err)
	}

	if timing.LoopCount != 5 {
		t.Fatalf("loop count = %d, want 5", timing.LoopCount)
	}

	if timing.OutputFrames != 10000 {
		t.Fatalf("output frames = %d, want 10000", timing.OutputFrames)
	}

	if timing.OutputDurationSeconds() < 1.1 {
		t.Fatalf("output %.3fs is shorter than the requested 1.1s", timing.OutputDurationSeconds())
	}
}

func TestPlanLoopForDurationExplicitLoopCountWins(t *testing.T) {
	timing, err := PlanLoopForDuration(4000, testRate, 10*time.Second, 250*time.Millisecond, 3)
	if err != nil {
		t.Fatalf("PlanLoopForDuration returned error: %v", err)
	}

	if timing.LoopCount != 3 {
		t.Fatalf("loop count = %d, want the explicit 3", timing.LoopCount)
	}

	if timing.OutputFrames != 3*2000 {
		t.Fatalf("output frames = %d, want 6000", timing.OutputFrames)
	}
}

// TestSeamlessLoopSeamIsContinuous checks that repeating the encoded file does
// not jump at the loop boundary. For a sine, the boundary jump must be no
// larger than the natural sample-to-sample step of the waveform.
func TestSeamlessLoopSeamIsContinuous(t *testing.T) {
	const frames = 800 // 0.1s at 8kHz
	const crossfade = 200
	const loops = 3

	// A non-integer number of cycles makes the original clip boundary a real
	// discontinuity, so the comparison against plain concatenation is
	// meaningful.
	clip := sineClip(t, frames, 1, 10.3)

	plain, _, err := BuildSeamlessLoop(clip, LoopOptions{LoopCount: loops})
	if err != nil {
		t.Fatalf("plain loop returned error: %v", err)
	}

	out, timing, err := BuildSeamlessLoop(clip, LoopOptions{
		LoopCount:       loops,
		CrossfadeFrames: crossfade,
	})
	if err != nil {
		t.Fatalf("BuildSeamlessLoop returned error: %v", err)
	}

	if timing.OutputFrames != loops*(frames-crossfade) {
		t.Fatalf("output frames = %d, want %d", timing.OutputFrames, loops*(frames-crossfade))
	}

	naturalStep := maxAdjacentStep(clip.Channels[0])
	samples := out.Channels[0]
	seamStep := math.Abs(float64(samples[0]) - float64(samples[len(samples)-1]))

	if seamStep > naturalStep*2+1e-6 {
		t.Fatalf(
			"seam step %.5f exceeds natural step %.5f; loop boundary would click",
			seamStep,
			naturalStep,
		)
	}

	// The plain concatenation still contains the clip's own boundary
	// discontinuity at every loop point, which the crossfaded render removes.
	plainSamples := plain.Channels[0]
	plainSeam := math.Abs(float64(plainSamples[frames]) - float64(plainSamples[frames-1]))
	plainNatural := naturalStep
	if plainSeam <= plainNatural*2 {
		t.Fatalf(
			"expected plain concatenation to produce a seam step, got %.5f (natural %.5f)",
			plainSeam,
			plainNatural,
		)
	}
}

// TestSeamlessLoopCrossfadeGainsMatchEqualPower checks the exact mix at known
// positions of the crossfade region.
func TestSeamlessLoopCrossfadeGainsMatchEqualPower(t *testing.T) {
	const frames = 400
	const crossfade = 100
	const loops = 2

	clip := PCMClip{SampleRate: testRate, Channels: make([][]float32, 2)}

	for channel := 0; channel < 2; channel++ {
		clip.Channels[channel] = make([]float32, frames)

		for frame := range clip.Channels[channel] {
			clip.Channels[channel][frame] = float32(frame) / float32(frames)
		}
	}

	out, _, err := BuildSeamlessLoop(clip, LoopOptions{
		LoopCount:       loops,
		CrossfadeFrames: crossfade,
	})
	if err != nil {
		t.Fatalf("BuildSeamlessLoop returned error: %v", err)
	}

	for _, q := range []int{0, 1, crossfade / 2, crossfade - 1} {
		fadeOut, fadeIn := equalPowerGains(q, crossfade)
		tail := float64(clip.Channels[0][frames-crossfade+q])
		head := float64(clip.Channels[0][q])
		want := clampSample(tail*fadeOut + head*fadeIn)

		if math.Abs(float64(out.Channels[0][q])-float64(want)) > 1e-6 {
			t.Fatalf(
				"crossfade frame %d = %.7f, want %.7f",
				q,
				out.Channels[0][q],
				want,
			)
		}
	}

	// Silence one channel in the source and confirm it stays silent.
	silent := PCMClip{
		SampleRate: testRate,
		Channels: [][]float32{
			clip.Channels[0],
			make([]float32, frames),
		},
	}

	silentOut, _, err := BuildSeamlessLoop(silent, LoopOptions{
		LoopCount:       loops,
		CrossfadeFrames: crossfade,
	})
	if err != nil {
		t.Fatalf("silent right channel returned error: %v", err)
	}

	for frame, value := range silentOut.Channels[1] {
		if value != 0 {
			t.Fatalf("right channel frame %d = %f, want silence", frame, value)
		}
	}
}

func maxAdjacentJump(samples []float32) float64 {
	largest := 0.0

	for i := 1; i < len(samples); i++ {
		jump := math.Abs(float64(samples[i]) - float64(samples[i-1]))
		if jump > largest {
			largest = jump
		}
	}

	return largest
}

// TestSeamlessLoopProcessesChannelsIndependently renders each channel alone
// and confirms the stereo render matches both independently.
func TestSeamlessLoopProcessesChannelsIndependently(t *testing.T) {
	const frames = 400
	const crossfade = 100
	const loops = 3

	left := sineClip(t, frames, 1, 4).Channels[0]
	right := make([]float32, frames)

	for frame := range right {
		right[frame] = -0.5 * sineClip(t, frames, 1, 4).Channels[0][frame]
	}

	stereo := PCMClip{
		SampleRate: testRate,
		Channels:   [][]float32{left, right},
	}

	out, _, err := BuildSeamlessLoop(stereo, LoopOptions{
		LoopCount:       loops,
		CrossfadeFrames: crossfade,
	})
	if err != nil {
		t.Fatalf("stereo BuildSeamlessLoop returned error: %v", err)
	}

	for channel, source := range [][]float32{left, right} {
		mono := PCMClip{SampleRate: testRate, Channels: [][]float32{source}}
		monoOut, _, err := BuildSeamlessLoop(mono, LoopOptions{
			LoopCount:       loops,
			CrossfadeFrames: crossfade,
		})
		if err != nil {
			t.Fatalf("mono BuildSeamlessLoop returned error: %v", err)
		}

		for frame := range monoOut.Channels[0] {
			if math.Abs(float64(out.Channels[channel][frame])-float64(monoOut.Channels[0][frame])) > 1e-6 {
				t.Fatalf(
					"channel %d frame %d = %f, independent render = %f",
					channel,
					frame,
					out.Channels[channel][frame],
					monoOut.Channels[0][frame],
				)
			}
		}
	}
}

func maxAdjacentStep(samples []float32) float64 {
	largest := 0.0

	for i := 1; i < len(samples); i++ {
		step := math.Abs(float64(samples[i]) - float64(samples[i-1]))
		if step > largest {
			largest = step
		}
	}

	return largest
}

func TestSeamlessLoopDisabledMatchesConcatenation(t *testing.T) {
	const frames = 120
	const loops = 3

	clip := sineClip(t, frames, 2, 3)

	out, timing, err := BuildSeamlessLoop(clip, LoopOptions{LoopCount: loops})
	if err != nil {
		t.Fatalf("BuildSeamlessLoop returned error: %v", err)
	}

	if timing.OutputFrames != frames*loops {
		t.Fatalf("output frames = %d, want %d", timing.OutputFrames, frames*loops)
	}

	for channel := range clip.Channels {
		for loop := 0; loop < loops; loop++ {
			for frame := 0; frame < frames; frame++ {
				got := out.Channels[channel][loop*frames+frame]
				want := clip.Channels[channel][frame]

				if got != want {
					t.Fatalf(
						"channel %d loop %d frame %d = %f, want %f",
						channel,
						loop,
						frame,
						got,
						want,
					)
				}
			}
		}
	}
}

func TestSeamlessLoopRejectsInvalidInput(t *testing.T) {
	clip := sineClip(t, 100, 1, 1)

	if _, _, err := BuildSeamlessLoop(clip, LoopOptions{LoopCount: 0}); !errors.Is(err, ErrInvalidLoopCount) {
		t.Fatalf("expected ErrInvalidLoopCount, got %v", err)
	}

	if _, _, err := BuildSeamlessLoop(clip, LoopOptions{LoopCount: 2, CrossfadeFrames: 51}); !errors.Is(err, ErrCrossfadeTooLong) {
		t.Fatalf("expected ErrCrossfadeTooLong, got %v", err)
	}

	empty := PCMClip{SampleRate: testRate, Channels: [][]float32{{}}}
	if _, _, err := BuildSeamlessLoop(empty, LoopOptions{LoopCount: 2}); !errors.Is(err, ErrEmptyClip) {
		t.Fatalf("expected ErrEmptyClip, got %v", err)
	}

	noTrack := PCMClip{SampleRate: testRate}
	if _, _, err := BuildSeamlessLoop(noTrack, LoopOptions{LoopCount: 2}); !errors.Is(err, ErrNoAudioTrack) {
		t.Fatalf("expected ErrNoAudioTrack, got %v", err)
	}

	invalidRate := PCMClip{SampleRate: 0, Channels: [][]float32{{1}}}
	if _, _, err := BuildSeamlessLoop(invalidRate, LoopOptions{LoopCount: 2}); !errors.Is(err, ErrInvalidAudio) {
		t.Fatalf("expected ErrInvalidAudio, got %v", err)
	}
}

func TestPlanLoopForDurationRejectsTooLongCrossfade(t *testing.T) {
	// 0.01s clip at 8kHz is 80 frames; a 0.02s crossfade is 160 frames.
	_, err := PlanLoopForDuration(80, testRate, 1_000_000_000, 20_000_000, 0)
	if !errors.Is(err, ErrCrossfadeTooLong) {
		t.Fatalf("expected ErrCrossfadeTooLong, got %v", err)
	}
}

// TestEqualPowerCrossfadePreservesPower mixes uncorrelated head and tail
// segments (different frequencies, equal RMS). With equal-power gains the RMS
// of the mix stays constant across the fade, while a linear crossfade would dip
// by up to 3 dB in the middle.
func TestEqualPowerCrossfadePreservesPower(t *testing.T) {
	const frames = 2000
	const crossfade = 400
	const loops = 3
	const amplitude = 0.8

	clip := PCMClip{SampleRate: testRate, Channels: make([][]float32, 1)}
	clip.Channels[0] = make([]float32, frames)

	for frame := 0; frame < frames; frame++ {
		// Head region: 300 Hz. Tail region: 700 Hz. Neither divides the other,
		// so the two segments are effectively uncorrelated.
		frequency := 300.0
		if frame >= frames-crossfade {
			frequency = 700.0
		}

		clip.Channels[0][frame] = float32(
			amplitude * math.Sin(2*math.Pi*frequency*float64(frame)/float64(testRate)),
		)
	}

	out, _, err := BuildSeamlessLoop(clip, LoopOptions{
		LoopCount:       loops,
		CrossfadeFrames: crossfade,
	})
	if err != nil {
		t.Fatalf("BuildSeamlessLoop returned error: %v", err)
	}

	// RMS of a single tone at this amplitude.
	wantRMS := amplitude / math.Sqrt2
	wantPower := wantRMS * wantRMS

	// Compare the middle of the crossfade with a later part of the clip body.
	mid := out.Channels[0][crossfade/4 : 3*crossfade/4]
	body := out.Channels[0][crossfade+200 : crossfade+400]

	midPower := meanPower(mid)
	bodyPower := meanPower(body)

	if math.Abs(midPower-wantPower) > 0.05*wantPower {
		t.Fatalf(
			"equal-power mix power = %.4f, want %.4f +- 5%%",
			midPower,
			wantPower,
		)
	}

	if math.Abs(midPower-bodyPower) > 0.1*bodyPower {
		t.Fatalf("mix power %.4f differs from body power %.4f", midPower, bodyPower)
	}

	// For contrast, a linear (amplitude) crossfade of the same signals dips:
	// the average power at the midpoint is about half.
	linearMid := make([]float32, len(mid))

	for i := 0; i < len(linearMid); i++ {
		position := i + crossfade/4
		linear := (1-float64(position)/crossfade)*float64(clip.Channels[0][frames-crossfade+position]) +
			(float64(position)/crossfade)*float64(clip.Channels[0][position])
		linearMid[i] = float32(linear)
	}

	if meanPower(linearMid) >= 0.75*midPower {
		t.Fatalf(
			"linear crossfade power %.4f should be clearly lower than equal-power %.4f",
			meanPower(linearMid),
			midPower,
		)
	}
}

func meanPower(samples []float32) float64 {
	if len(samples) == 0 {
		return 0
	}

	total := 0.0
	for _, sample := range samples {
		total += float64(sample) * float64(sample)
	}

	return total / float64(len(samples))
}
