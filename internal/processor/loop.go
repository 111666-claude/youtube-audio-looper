package processor

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

var (
	// ErrCrossfadeTooLong means the requested crossfade does not fit inside
	// the clip. A crossfade consumes the clip's head and tail, so at most half
	// of the clip can be used.
	ErrCrossfadeTooLong = errors.New("crossfade is longer than the clip")
	// ErrInvalidLoopCount means the requested loop count is not positive.
	ErrInvalidLoopCount = errors.New("loop count must be greater than zero")
)

// LoopOptions controls seamless loop generation.
type LoopOptions struct {
	// LoopCount is the number of loop units written to the output.
	LoopCount int
	// CrossfadeFrames is the equal-power crossfade length in frames. Zero
	// keeps the original hard concatenation.
	CrossfadeFrames int
}

// LoopTiming describes the resulting timeline.
type LoopTiming struct {
	SampleRate      int
	ClipFrames      int
	LoopCount       int
	CrossfadeFrames int
	// UnitFrames is the length of one loop unit: clip - crossfade.
	UnitFrames int
	// OutputFrames is LoopCount * UnitFrames.
	OutputFrames int
}

// UnitDurationSeconds returns the duration of one loop unit.
func (t LoopTiming) UnitDurationSeconds() float64 {
	if t.SampleRate <= 0 {
		return 0
	}

	return float64(t.UnitFrames) / float64(t.SampleRate)
}

// OutputDurationSeconds returns the final output duration.
func (t LoopTiming) OutputDurationSeconds() float64 {
	if t.SampleRate <= 0 {
		return 0
	}

	return float64(t.OutputFrames) / float64(t.SampleRate)
}

// LoopCountForTarget returns the smallest loop count that covers targetFrames
// when each unit is unitFrames long:
//
//	N = max(1, ceil(D / P))
//
// For P == clip (crossfade disabled) this is the plain ceil(D / clip).
func LoopCountForTarget(unitFrames, targetFrames int) int {
	if unitFrames <= 0 {
		return 0
	}

	if targetFrames <= 0 {
		return 1
	}

	count := (targetFrames + unitFrames - 1) / unitFrames
	if count < 1 {
		return 1
	}

	return count
}

// PlanLoop validates options against a clip and returns the timeline.
//
// The tail and head of the clip are mixed with equal-power gains into a
// circular loop unit:
//
//	P = C - F
//	output = N * P
//
// where C is the clip length, F the crossfade length, and N the loop count,
// all in frames. With F == 0 this reduces to N * C, the original hard
// concatenation.
//
// Every seam, including the boundary between the end of the file and its
// beginning, falls inside the same crossfade, so replaying the file is
// seamless as well. F must not exceed half of the clip: the fade consumes both
// the head and the tail, so a longer fade would overlap more than two copies.
func PlanLoop(clipFrames, sampleRate, loopCount, crossfadeFrames int) (LoopTiming, error) {
	timing := LoopTiming{
		SampleRate:      sampleRate,
		ClipFrames:      clipFrames,
		LoopCount:       loopCount,
		CrossfadeFrames: crossfadeFrames,
	}

	if sampleRate <= 0 || clipFrames <= 0 {
		return timing, ErrEmptyClip
	}

	if loopCount <= 0 {
		return timing, ErrInvalidLoopCount
	}

	if crossfadeFrames < 0 {
		return timing, fmt.Errorf("%w: negative crossfade", ErrCrossfadeTooLong)
	}

	if crossfadeFrames*2 > clipFrames {
		return timing, fmt.Errorf(
			"%w: crossfade %d frames, clip %d frames (maximum is half the clip)",
			ErrCrossfadeTooLong,
			crossfadeFrames,
			clipFrames,
		)
	}

	timing.UnitFrames = clipFrames - crossfadeFrames
	timing.OutputFrames = loopCount * timing.UnitFrames

	if timing.OutputFrames <= 0 {
		return timing, fmt.Errorf("%w: output would be empty", ErrInvalidLoopCount)
	}

	return timing, nil
}

// PlanLoopForDuration converts time-based options into frames and derives the
// loop count from the requested final duration:
//
//	F = round(crossfade * sampleRate)
//	D = ceil(duration * sampleRate)
//	P = C - F
//	N = max(1, ceil(D / P))
//	output = N * P
//
// The output therefore covers the requested duration and is an exact whole
// number of loop units, which is what keeps the file's own loop point
// seamless. If loopCount is positive it overrides the derived count.
func PlanLoopForDuration(
	clipFrames, sampleRate int,
	targetDuration, crossfadeDuration time.Duration,
	loopCount int,
) (LoopTiming, error) {
	emptyTiming := LoopTiming{SampleRate: sampleRate, ClipFrames: clipFrames}

	if sampleRate <= 0 || clipFrames <= 0 {
		return emptyTiming, ErrEmptyClip
	}

	if targetDuration <= 0 {
		return emptyTiming, ErrInvalidLoopCount
	}

	if crossfadeDuration < 0 {
		return emptyTiming, fmt.Errorf("%w: negative crossfade", ErrCrossfadeTooLong)
	}

	seconds := float64(sampleRate)
	fadeFrames := int(math.Round(crossfadeDuration.Seconds() * seconds))

	if fadeFrames*2 > clipFrames {
		return LoopTiming{
			SampleRate:      sampleRate,
			ClipFrames:      clipFrames,
			CrossfadeFrames: fadeFrames,
		}, fmt.Errorf(
			"%w: crossfade %.3fs, clip %.3fs (maximum is half the clip)",
			ErrCrossfadeTooLong,
			crossfadeDuration.Seconds(),
			float64(clipFrames)/seconds,
		)
	}

	if loopCount <= 0 {
		// Round the target up so the output never ends before the requested
		// time.
		targetFrames := int(math.Ceil(targetDuration.Seconds() * seconds))
		loopCount = LoopCountForTarget(clipFrames-fadeFrames, targetFrames)
	}

	return PlanLoop(clipFrames, sampleRate, loopCount, fadeFrames)
}

// equalPowerGains returns the fade-out and fade-in gains for crossfade frame i
// of total. The cosine/sine pair keeps the summed power constant, so an
// uncorrelated signal does not dip in the middle of the fade; the gains are
// applied identically to every channel.
func equalPowerGains(i, total int) (float64, float64) {
	if total <= 0 {
		return 1, 1
	}

	position := (float64(i) + 0.5) / float64(total)
	angle := position * math.Pi / 2

	return math.Cos(angle), math.Sin(angle)
}

// unitSample returns one frame of the loop unit:
//
//	u[q] = clip[C-F+q]*fadeOut(q) + clip[q]*fadeIn(q)   for q < F
//	u[q] = clip[q]                                     for F <= q < P
//
// so the unit is a seamless circular mix of the clip's tail into its head.
func unitSample(source []float32, q, clipFrames, fadeFrames int) float32 {
	if q < fadeFrames {
		fadeOut, fadeIn := equalPowerGains(q, fadeFrames)
		tail := float64(source[clipFrames-fadeFrames+q])
		head := float64(source[q])

		return clampSample(tail*fadeOut + head*fadeIn)
	}

	return source[q]
}

// RenderLoop writes interleaved little-endian float32 samples for a seamless
// loop. Each channel is mixed independently with the same gains.
//
// The output is the loop unit repeated LoopCount times. See PlanLoop for the
// length formula.
func RenderLoop(ctx context.Context, clip PCMClip, opts LoopOptions, w io.Writer) (LoopTiming, error) {
	if err := clip.validate(); err != nil {
		return LoopTiming{}, err
	}

	timing, err := PlanLoop(clip.SampleCount(), clip.SampleRate, opts.LoopCount, opts.CrossfadeFrames)
	if err != nil {
		return timing, err
	}

	channelCount := len(clip.Channels)
	clipFrames := timing.ClipFrames
	fadeFrames := timing.CrossfadeFrames
	unitFrames := timing.UnitFrames
	outputFrames := timing.OutputFrames

	const chunkFrames = 4096
	buffer := make([]byte, chunkFrames*channelCount*4)

	for start := 0; start < outputFrames; start += chunkFrames {
		if err := ctx.Err(); err != nil {
			return timing, err
		}

		frames := outputFrames - start
		if frames > chunkFrames {
			frames = chunkFrames
		}

		for frame := 0; frame < frames; frame++ {
			position := (start + frame) % unitFrames

			for channel := 0; channel < channelCount; channel++ {
				value := unitSample(clip.Channels[channel], position, clipFrames, fadeFrames)
				offset := (frame*channelCount + channel) * 4
				binary.LittleEndian.PutUint32(buffer[offset:], math.Float32bits(value))
			}
		}

		if _, err := w.Write(buffer[:frames*channelCount*4]); err != nil {
			return timing, err
		}
	}

	return timing, nil
}

// BuildSeamlessLoop renders a seamless loop in memory. It is useful for tests
// and for callers that already hold the whole PCM clip; the service streams
// long outputs to disk through RenderLoop instead.
func BuildSeamlessLoop(clip PCMClip, opts LoopOptions) (PCMClip, LoopTiming, error) {
	var buffer bytes.Buffer

	timing, err := RenderLoop(context.Background(), clip, opts, &buffer)
	if err != nil {
		return PCMClip{}, timing, err
	}

	channelCount := len(clip.Channels)
	frameSize := channelCount * 4
	data := buffer.Bytes()

	if len(data) != timing.OutputFrames*frameSize {
		return PCMClip{}, timing, fmt.Errorf(
			"%w: rendered %d bytes, expected %d",
			ErrInvalidAudio,
			len(data),
			timing.OutputFrames*frameSize,
		)
	}

	output := PCMClip{
		SampleRate: clip.SampleRate,
		Channels:   make([][]float32, channelCount),
	}

	for channel := range output.Channels {
		output.Channels[channel] = make([]float32, timing.OutputFrames)

		for frame := 0; frame < timing.OutputFrames; frame++ {
			offset := (frame*channelCount + channel) * 4
			output.Channels[channel][frame] = math.Float32frombits(
				binary.LittleEndian.Uint32(data[offset:]),
			)
		}
	}

	return output, timing, nil
}
