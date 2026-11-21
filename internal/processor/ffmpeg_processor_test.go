package processor

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()

	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is not installed", binary)
		}
	}
}

// writeSineMP3 creates a stereo MP3 whose channels have different frequencies,
// which makes channel swaps visible after the round trip.
func writeSineMP3(t *testing.T, path string, seconds float64) {
	t.Helper()

	cmd := exec.Command(
		"ffmpeg",
		"-y",
		"-v", "error",
		"-f", "lavfi",
		"-i", "sine=frequency=440:duration="+formatSeconds(seconds)+":sample_rate=44100",
		"-f", "lavfi",
		"-i", "sine=frequency=880:duration="+formatSeconds(seconds)+":sample_rate=44100",
		"-filter_complex", "[0:a][1:a]amerge=inputs=2[a]",
		"-map", "[a]",
		"-ac", "2",
		"-acodec", "libmp3lame",
		"-q:a", "2",
		path,
	)

	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create test mp3: %v, output: %s", err, output)
	}
}

func formatSeconds(seconds float64) string {
	return time.Duration(seconds * float64(time.Second)).String()
}

func probeDurationSeconds(t *testing.T, path string) float64 {
	t.Helper()

	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe duration: %v, output: %s", err, output)
	}

	text := strings.TrimSpace(string(output))
	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil {
		t.Fatalf("parse duration %q: %v", text, err)
	}

	return seconds
}

func TestLoopAudioSeamlessEndToEnd(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp3")
	output := filepath.Join(dir, "result.mp3")

	writeSineMP3(t, input, 2)

	processor := NewFFmpegProcessor()

	timing, err := processor.LoopAudioSeamless(
		context.Background(),
		input,
		output,
		5*time.Second,
		500*time.Millisecond,
		0,
	)
	if err != nil {
		t.Fatalf("LoopAudioSeamless returned error: %v", err)
	}

	if timing.SampleRate != OutputSampleRate {
		t.Fatalf("sample rate = %d, want %d", timing.SampleRate, OutputSampleRate)
	}

	if timing.CrossfadeFrames != OutputSampleRate/2 {
		t.Fatalf("crossfade frames = %d, want %d", timing.CrossfadeFrames, OutputSampleRate/2)
	}

	if timing.OutputFrames < OutputSampleRate*5 {
		t.Fatalf("output frames = %d, want at least %d", timing.OutputFrames, OutputSampleRate*5)
	}

	// The output is a whole number of loop units, so it rounds up by less than
	// one unit (1.5s here).
	if timing.OutputFrames > OutputSampleRate*5+timing.UnitFrames {
		t.Fatalf(
			"output frames = %d, overshoot beyond one unit (%d)",
			timing.OutputFrames,
			timing.UnitFrames,
		)
	}

	duration := probeDurationSeconds(t, output)
	minDuration := float64(timing.OutputFrames) / OutputSampleRate
	if duration < minDuration-0.05 || duration > minDuration+0.1 {
		t.Fatalf("encoded duration = %.3fs, want ~%.3fs", duration, minDuration)
	}

	// Decode the encoded result and check both channels still carry their own
	// frequency: the left stays near 0.25 duty at 440 Hz and the right at
	// 880 Hz. A channel swap would make the right's zero-crossing count double.
	decoded, err := processor.DecodePCM(context.Background(), output)
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}

	if len(decoded.Channels) != 2 {
		t.Fatalf("decoded channels = %d, want 2", len(decoded.Channels))
	}

	leftCrossings := zeroCrossings(decoded.Channels[0])
	rightCrossings := zeroCrossings(decoded.Channels[1])

	if rightCrossings < leftCrossings*3/2 {
		t.Fatalf(
			"channel frequencies collapsed: left crossings %d, right crossings %d",
			leftCrossings,
			rightCrossings,
		)
	}
}

func TestLoopAudioSeamlessDisabledMatchesPlainConcatenation(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp3")

	writeSineMP3(t, input, 1)

	processor := NewFFmpegProcessor()

	seamlessPath := filepath.Join(dir, "seamless.mp3")
	plainPath := filepath.Join(dir, "plain.mp3")

	if _, err := processor.LoopAudioSeamless(context.Background(), input, seamlessPath, 3*time.Second, 0, 0); err != nil {
		t.Fatalf("zero-crossfade seamless loop: %v", err)
	}

	if err := processor.LoopAudio(context.Background(), input, plainPath, "00:03"); err != nil {
		t.Fatalf("plain loop: %v", err)
	}

	seamless, err := processor.DecodePCM(context.Background(), seamlessPath)
	if err != nil {
		t.Fatalf("decode seamless: %v", err)
	}

	plain, err := processor.DecodePCM(context.Background(), plainPath)
	if err != nil {
		t.Fatalf("decode plain: %v", err)
	}

	if got, want := seamless.SampleCount(), 3*OutputSampleRate; got != want {
		t.Fatalf("seamless sample count = %d, want %d", got, want)
	}

	// MP3 encoding of identical PCM through two routes is not bit-exact, so
	// compare durations and coarse energy instead of samples.
	// MP3 frames are 26ms and the two encode paths pad differently, so allow
	// two frames of drift.
	if delta := seamless.DurationSeconds() - plain.DurationSeconds(); delta < -0.06 || delta > 0.06 {
		t.Fatalf("duration mismatch: seamless %.3fs, plain %.3fs", seamless.DurationSeconds(), plain.DurationSeconds())
	}

	if math.Abs(rms(seamless.Channels[0])-rms(plain.Channels[0])) > 0.05 {
		t.Fatalf("left channel energy differs: %.3f vs %.3f", rms(seamless.Channels[0]), rms(plain.Channels[0]))
	}
}

func TestLoopAudioSeamlessNoAudioTrack(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	input := filepath.Join(dir, "video-only.mp4")
	output := filepath.Join(dir, "result.mp3")

	cmd := exec.Command(
		"ffmpeg",
		"-y",
		"-v", "error",
		"-f", "lavfi",
		"-i", "color=c=black:s=32x32:d=1",
		"-an",
		input,
	)

	if err := cmd.Run(); err != nil {
		t.Fatalf("create video-only input: %v", err)
	}

	processor := NewFFmpegProcessor()

	_, err := processor.LoopAudioSeamless(context.Background(), input, output, time.Second, 0, 0)
	if !errors.Is(err, ErrNoAudioTrack) {
		t.Fatalf("expected ErrNoAudioTrack, got %v", err)
	}

	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output file should not exist after failure, stat err = %v", err)
	}
}

func TestLoopAudioSeamlessRejectsCrossfadeLongerThanClip(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp3")
	output := filepath.Join(dir, "result.mp3")

	writeSineMP3(t, input, 1)

	processor := NewFFmpegProcessor()

	_, err := processor.LoopAudioSeamless(context.Background(), input, output, 2*time.Second, 900*time.Millisecond, 0)
	if !errors.Is(err, ErrCrossfadeTooLong) {
		t.Fatalf("expected ErrCrossfadeTooLong, got %v", err)
	}

	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output file should not exist after failure, stat err = %v", err)
	}
}

func TestLoopAudioSeamlessCanceledContext(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp3")
	output := filepath.Join(dir, "result.mp3")

	writeSineMP3(t, input, 2)

	processor := NewFFmpegProcessor()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := processor.LoopAudioSeamless(ctx, input, output, 5*time.Second, 500*time.Millisecond, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output file should not exist after cancellation, stat err = %v", err)
	}
}

func TestEncodePCMRemovesPartialOutputOnCancel(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	output := filepath.Join(dir, "result.mp3")

	clip := sineClip(t, OutputSampleRate*10, 2, 100)
	processor := NewFFmpegProcessor()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := processor.EncodePCM(ctx, clip, LoopOptions{LoopCount: 3, CrossfadeFrames: 1000}, output)
	if err == nil {
		t.Fatalf("expected an error from a canceled context")
	}

	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial output should be removed, stat err = %v", err)
	}
}

func zeroCrossings(samples []float32) int {
	count := 0

	for i := 1; i < len(samples); i++ {
		if (samples[i-1] < 0 && samples[i] >= 0) || (samples[i-1] >= 0 && samples[i] < 0) {
			count++
		}
	}

	return count
}

func rms(samples []float32) float64 {
	if len(samples) == 0 {
		return 0
	}

	total := 0.0
	for _, sample := range samples {
		total += float64(sample) * float64(sample)
	}

	return math.Sqrt(total / float64(len(samples)))
}

// TestLoopAudioSeamlessVeryShortClip checks an input barely longer than the
// crossfade: the fade is clamped to half the clip and the result still covers
// the requested duration.
func TestLoopAudioSeamlessVeryShortClip(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp3")
	output := filepath.Join(dir, "result.mp3")

	writeSineMP3(t, input, 0.2)

	processor := NewFFmpegProcessor()

	// 50ms crossfade against a ~200ms clip is legal (<= half).
	timing, err := processor.LoopAudioSeamless(
		context.Background(),
		input,
		output,
		1*time.Second,
		50*time.Millisecond,
		0,
	)
	if err != nil {
		t.Fatalf("LoopAudioSeamless returned error: %v", err)
	}

	if timing.CrossfadeFrames > timing.ClipFrames/2 {
		t.Fatalf(
			"crossfade %d frames exceeds half of clip %d",
			timing.CrossfadeFrames,
			timing.ClipFrames,
		)
	}

	if timing.OutputFrames < OutputSampleRate {
		t.Fatalf("output frames = %d, want at least 1s", timing.OutputFrames)
	}

	if duration := probeDurationSeconds(t, output); duration < 0.9 {
		t.Fatalf("encoded duration = %.3fs, want at least ~1s", duration)
	}
}

// TestLoopAudioSeamlessExplicitLoopCount verifies that an explicit loop count
// overrides the duration-derived count.
func TestLoopAudioSeamlessExplicitLoopCount(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp3")
	output := filepath.Join(dir, "result.mp3")

	writeSineMP3(t, input, 1)

	processor := NewFFmpegProcessor()

	timing, err := processor.LoopAudioSeamless(
		context.Background(),
		input,
		output,
		100*time.Second,
		200*time.Millisecond,
		5,
	)
	if err != nil {
		t.Fatalf("LoopAudioSeamless returned error: %v", err)
	}

	if timing.LoopCount != 5 {
		t.Fatalf("loop count = %d, want the explicit 5", timing.LoopCount)
	}

	wantFrames := 5 * timing.UnitFrames
	if timing.OutputFrames != wantFrames {
		t.Fatalf("output frames = %d, want %d", timing.OutputFrames, wantFrames)
	}

	if duration := probeDurationSeconds(t, output); duration > 10 {
		t.Fatalf("explicit loop count should ignore the 100s duration, got %.3fs", duration)
	}
}
