package processor

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"time"
)

const (
	// OutputSampleRate is the decode/encode sample rate used by the seamless
	// path. It matches the rate yt-dlp normally produces, so the only
	// resampling that happens is ffmpeg's high-quality conversion.
	OutputSampleRate = 44100
	// OutputChannels is the channel count written to the MP3 encoder. Left
	// and right are decoded and mixed separately.
	OutputChannels = 2
)

type FFmpegProcessor struct {
}

func NewFFmpegProcessor() *FFmpegProcessor {
	return &FFmpegProcessor{}
}

func (p *FFmpegProcessor) CutAudio(ctx context.Context, inputPath, outputPath, start, end string) error {
	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-y",
		"-i", inputPath,
		"-ss", start,
		"-to", end,
		"-vn",
		"-acodec", "libmp3lame",
		"-q:a", "2",
		outputPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg cut failed: %w, output: %s", err, string(output))
	}

	return nil
}

// LoopAudio is the original hard-concatenation path. It is used when seamless
// looping is disabled and preserves the previous output exactly.
func (p *FFmpegProcessor) LoopAudio(ctx context.Context, inputPath string, outputPath string, duration string) error {
	// Report a missing audio track the same way on both paths, instead of
	// surfacing a generic ffmpeg failure.
	if err := checkAudioStream(ctx, inputPath); err != nil {
		return err
	}

	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-y",
		"-stream_loop", "-1",
		"-i", inputPath,
		"-t", duration,
		"-vn",
		"-acodec", "libmp3lame",
		"-q:a", "2",
		outputPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg loop failed: %w, output: %s", err, string(output))
	}

	return nil
}

// LoopAudioSeamless decodes inputPath, builds an equal-power tail/head loop,
// and encodes the result to outputPath.
//
// The number of loops is derived from the decoded clip length, the requested
// duration, and the crossfade length; see PlanLoopForDuration for the exact
// formulas. A zero crossfade produces plain clip concatenation.
func (p *FFmpegProcessor) LoopAudioSeamless(
	ctx context.Context,
	inputPath, outputPath string,
	targetDuration, crossfade time.Duration,
	loopCount int,
) (LoopTiming, error) {
	clip, err := p.DecodePCM(ctx, inputPath)
	if err != nil {
		return LoopTiming{}, err
	}

	timing, err := PlanLoopForDuration(
		clip.SampleCount(),
		clip.SampleRate,
		targetDuration,
		crossfade,
		loopCount,
	)
	if err != nil {
		return timing, err
	}

	if err := p.EncodePCM(ctx, clip, LoopOptions{
		LoopCount:       timing.LoopCount,
		CrossfadeFrames: timing.CrossfadeFrames,
	}, outputPath); err != nil {
		return timing, err
	}

	return timing, nil
}

// DecodePCM decodes the first audio stream into planar float32 samples at
// OutputSampleRate and OutputChannels. A missing audio stream is reported as
// ErrNoAudioTrack and undecodable input as ErrInvalidAudio.
func (p *FFmpegProcessor) DecodePCM(ctx context.Context, inputPath string) (PCMClip, error) {
	if err := checkAudioStream(ctx, inputPath); err != nil {
		return PCMClip{}, err
	}

	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-nostdin",
		"-v", "error",
		"-i", inputPath,
		"-vn",
		"-map", "0:a:0",
		"-ac", fmt.Sprintf("%d", OutputChannels),
		"-ar", fmt.Sprintf("%d", OutputSampleRate),
		"-acodec", "pcm_f32le",
		"-f", "f32le",
		"pipe:1",
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return PCMClip{}, fmt.Errorf("%w: stdout pipe: %v", ErrInvalidAudio, err)
	}

	if err := cmd.Start(); err != nil {
		return PCMClip{}, fmt.Errorf("%w: ffmpeg start: %v", ErrInvalidAudio, err)
	}

	data, readErr := io.ReadAll(stdout)
	waitErr := cmd.Wait()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return PCMClip{}, ctxErr
	}

	if readErr != nil {
		return PCMClip{}, fmt.Errorf("%w: read decoded audio: %v", ErrInvalidAudio, readErr)
	}

	if waitErr != nil {
		return PCMClip{}, fmt.Errorf(
			"%w: ffmpeg decode: %v, output: %s",
			ErrInvalidAudio,
			waitErr,
			stderr.String(),
		)
	}

	return pcmFromInterleaved(data)
}

// EncodePCM renders the loop and pipes it straight into the MP3 encoder, so
// long outputs never have to be buffered in memory. On any failure the output
// file is removed, including when ctx is canceled.
func (p *FFmpegProcessor) EncodePCM(
	ctx context.Context,
	clip PCMClip,
	opts LoopOptions,
	outputPath string,
) error {
	if err := clip.validate(); err != nil {
		return err
	}

	// A previous partial output must never be mistaken for a fresh result.
	_ = os.Remove(outputPath)

	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-nostdin",
		"-y",
		"-v", "error",
		"-f", "f32le",
		"-ar", fmt.Sprintf("%d", clip.SampleRate),
		"-ac", fmt.Sprintf("%d", len(clip.Channels)),
		"-i", "pipe:0",
		"-acodec", "libmp3lame",
		"-q:a", "2",
		"-f", "mp3",
		outputPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	_, renderErr := RenderLoop(ctx, clip, opts, stdin)
	closeErr := stdin.Close()

	if renderErr != nil {
		// Stop the encoder so it cannot wait forever for more input.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		_ = os.Remove(outputPath)

		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		return renderErr
	}

	if closeErr != nil {
		_ = cmd.Wait()
		_ = os.Remove(outputPath)

		return fmt.Errorf("close ffmpeg input: %w", closeErr)
	}

	if err := cmd.Wait(); err != nil {
		_ = os.Remove(outputPath)

		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		return fmt.Errorf("ffmpeg encode failed: %w, output: %s", err, stderr.String())
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("encoded audio missing: %w", err)
	}

	if info.Size() == 0 {
		_ = os.Remove(outputPath)

		return fmt.Errorf("%w: encoded audio is empty", ErrInvalidAudio)
	}

	return nil
}

func checkAudioStream(ctx context.Context, inputPath string) error {
	cmd := exec.CommandContext(
		ctx,
		"ffprobe",
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_type",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inputPath,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		return fmt.Errorf(
			"%w: ffprobe failed: %v, output: %s",
			ErrInvalidAudio,
			err,
			stderr.String(),
		)
	}

	if stdout.Len() == 0 {
		return ErrNoAudioTrack
	}

	return nil
}

func pcmFromInterleaved(data []byte) (PCMClip, error) {
	const channels = OutputChannels
	frameSize := channels * 4

	if len(data) == 0 {
		return PCMClip{}, ErrEmptyClip
	}

	if len(data)%frameSize != 0 {
		return PCMClip{}, fmt.Errorf(
			"%w: decoded audio is not aligned to %d-byte frames",
			ErrInvalidAudio,
			frameSize,
		)
	}

	frames := len(data) / frameSize
	clip := PCMClip{
		SampleRate: OutputSampleRate,
		Channels:   make([][]float32, channels),
	}

	for channel := range clip.Channels {
		clip.Channels[channel] = make([]float32, frames)
	}

	for frame := 0; frame < frames; frame++ {
		for channel := 0; channel < channels; channel++ {
			offset := (frame*channels + channel) * 4
			bits := binary.LittleEndian.Uint32(data[offset:])
			clip.Channels[channel][frame] = math.Float32frombits(bits)
		}
	}

	return clip, nil
}
