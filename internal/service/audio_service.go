package service

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/cobrich/youtube-audio-looper/internal/downloader"
	"github.com/cobrich/youtube-audio-looper/internal/entity"
	"github.com/cobrich/youtube-audio-looper/internal/processor"
	"github.com/cobrich/youtube-audio-looper/internal/utils"
)

// SegmentDownloader downloads a single audio segment from a video URL.
type SegmentDownloader interface {
	DownloadAudioSegment(ctx context.Context, url, outputPath, start, end string) error
}

// AudioProcessor turns the downloaded segment into a looped MP3.
type AudioProcessor interface {
	LoopAudio(ctx context.Context, inputPath, outputPath, duration string) error
	LoopAudioSeamless(
		ctx context.Context,
		inputPath, outputPath string,
		targetDuration, crossfade time.Duration,
		loopCount int,
	) (processor.LoopTiming, error)
}

// Result is the generated audio plus the timeline it was built from.
type Result struct {
	Data []byte
	// Timing is nil when seamless looping is disabled, because the plain path
	// is driven by ffmpeg's stream_loop and has no decoded timeline.
	Timing *processor.LoopTiming
	// Seamless reports which path produced the audio.
	Seamless bool
	// Crossfade is the effective crossfade length in seconds.
	Crossfade float64
}

type AudioService struct {
	downloader SegmentDownloader
	processor  AudioProcessor
	jobManager *JobManager
}

func NewAudioService() *AudioService {
	return newAudioService(
		downloader.NewYouTubeDownloader(),
		processor.NewFFmpegProcessor(),
		NewJobManager("tmp/jobs"),
	)
}

// newAudioService allows tests to substitute the downloader and processor.
func newAudioService(
	downloader SegmentDownloader,
	audioProcessor AudioProcessor,
	jobManager *JobManager,
) *AudioService {
	return &AudioService{
		downloader: downloader,
		processor:  audioProcessor,
		jobManager: jobManager,
	}
}

// CreateLoopedAudio downloads the requested segment and returns the looped MP3.
// The job directory is always removed, including when the request context is
// canceled or a step fails, so partial downloads and encodes do not accumulate.
func (s *AudioService) CreateLoopedAudio(ctx context.Context, req entity.Request) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	job, err := s.jobManager.Create()
	if err != nil {
		return nil, err
	}

	seamless := req.SeamlessEnabled()
	crossfade := req.CrossfadeSeconds()
	if !seamless {
		crossfade = 0
	}

	crossfadeDuration := time.Duration(crossfade * float64(time.Second))

	log.Printf(
		"job=%s youtube=%s start=%s end=%s duration=%s seamless=%t crossfade=%.3fs",
		job.ID,
		req.YoutubeURL,
		req.Start,
		req.End,
		req.Duration,
		seamless,
		crossfade,
	)

	// Every return path, including cancellation and panics, removes the job
	// directory with the downloaded clip and any partial output.
	defer func() {
		if err := os.RemoveAll(job.Dir); err != nil {
			log.Printf("job=%s cleanup failed: %v", job.ID, err)
			return
		}

		log.Printf("job=%s finished", job.ID)
	}()

	clipPath := filepath.Join(job.Dir, "clip.mp3")
	resultPath := filepath.Join(job.Dir, "result.mp3")

	log.Printf("job=%s downloading audio segment", job.ID)
	startedAt := time.Now()
	if err := s.downloader.DownloadAudioSegment(ctx, req.YoutubeURL, clipPath, req.Start, req.End); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			log.Printf("job=%s download canceled: %v", job.ID, ctxErr)
			return nil, ctxErr
		}

		return nil, err
	}
	log.Printf("job=%s downloaded audio segment in %s", job.ID, time.Since(startedAt))

	targetDuration, err := durationFromTimestamp(req.Duration)
	if err != nil {
		return nil, err
	}

	log.Printf(
		"job=%s looping audio seamless=%t crossfade=%.3fs",
		job.ID,
		seamless,
		crossfade,
	)
	startedAt = time.Now()

	if !seamless {
		// Seamless looping disabled: keep the original unmodified
		// concatenation behavior and output.
		if err := s.processor.LoopAudio(ctx, clipPath, resultPath, req.Duration); err != nil {
			return nil, s.contextErrorOr(ctx, job.ID, "processing", err)
		}

		log.Printf("job=%s looped audio (plain concatenation) in %s", job.ID, time.Since(startedAt))

		data, err := s.readResult(ctx, resultPath)
		if err != nil {
			return nil, err
		}

		return &Result{Data: data, Seamless: false, Crossfade: 0}, nil
	}

	timing, err := s.processor.LoopAudioSeamless(
		ctx,
		clipPath,
		resultPath,
		targetDuration,
		crossfadeDuration,
		req.LoopCount(),
	)
	if err != nil {
		return nil, s.contextErrorOr(ctx, job.ID, "processing", err)
	}

	result := &Result{
		Timing:    &timing,
		Seamless:  true,
		Crossfade: crossfade,
	}

	log.Printf(
		"job=%s looped audio in %s clip=%.3fs crossfade=%.3fs unit=%.3fs loops=%d output=%.3fs",
		job.ID,
		time.Since(startedAt),
		float64(timing.ClipFrames)/float64(timing.SampleRate),
		float64(timing.CrossfadeFrames)/float64(timing.SampleRate),
		timing.UnitDurationSeconds(),
		timing.LoopCount,
		timing.OutputDurationSeconds(),
	)

	data, err := s.readResult(ctx, resultPath)
	if err != nil {
		return nil, err
	}

	result.Data = data

	return result, nil
}

// contextErrorOr maps a step failure to the context error when the request was
// canceled or timed out, so callers can distinguish cancellation from a real
// processing failure.
func (s *AudioService) contextErrorOr(
	ctx context.Context,
	jobID, step string,
	err error,
) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		log.Printf("job=%s %s canceled: %v", jobID, step, ctxErr)

		return ctxErr
	}

	return err
}

func (s *AudioService) readResult(ctx context.Context, resultPath string) ([]byte, error) {
	data, err := os.ReadFile(resultPath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}

		return nil, err
	}

	return data, nil
}

// durationFromTimestamp converts an API timestamp (MM:SS or HH:MM:SS) into a
// time.Duration. It reuses the same parser as validation so the API and the web
// form accept identical values.
func durationFromTimestamp(value string) (time.Duration, error) {
	seconds, err := utils.ParseTimeToSeconds(value)
	if err != nil {
		return 0, err
	}

	if seconds <= 0 {
		return 0, errors.New("duration must be grater than zero")
	}

	return time.Duration(seconds) * time.Second, nil
}
