package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cobrich/youtube-audio-looper/internal/entity"
	"github.com/cobrich/youtube-audio-looper/internal/processor"
)

type fakeDownloader struct {
	err      error
	blockCh  chan struct{}
	onStart  func(outputPath string)
	started  chan struct{}
	startDel time.Duration
}

func (d *fakeDownloader) DownloadAudioSegment(ctx context.Context, url, outputPath, start, end string) error {
	if d.started != nil {
		close(d.started)
	}

	if d.onStart != nil {
		d.onStart(outputPath)
	}

	if d.blockCh != nil {
		select {
		case <-d.blockCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if d.startDel > 0 {
		select {
		case <-time.After(d.startDel):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return d.err
}

type fakeProcessor struct {
	err        error
	calls      int
	seamless   bool
	crossfade  time.Duration
	loopCount  int
	onSeamless func(outputPath string)
}

func (p *fakeProcessor) LoopAudio(ctx context.Context, inputPath, outputPath, duration string) error {
	p.calls++

	if p.err != nil {
		return p.err
	}

	return os.WriteFile(outputPath, []byte("plain"), 0644)
}

func (p *fakeProcessor) LoopAudioSeamless(
	ctx context.Context,
	inputPath, outputPath string,
	targetDuration, crossfade time.Duration,
	loopCount int,
) (processor.LoopTiming, error) {
	p.calls++
	p.seamless = true
	p.crossfade = crossfade
	p.loopCount = loopCount

	if p.onSeamless != nil {
		p.onSeamless(outputPath)
	}

	if p.err != nil {
		return processor.LoopTiming{SampleRate: 44100, LoopCount: 2}, p.err
	}

	if err := os.WriteFile(outputPath, []byte("seamless"), 0644); err != nil {
		return processor.LoopTiming{}, err
	}

	return processor.LoopTiming{SampleRate: 44100, LoopCount: 2}, nil
}

func newTestService(t *testing.T, dl SegmentDownloader, proc AudioProcessor) *AudioService {
	t.Helper()

	return newAudioService(dl, proc, NewJobManager(t.TempDir()))
}

func enabledRequest() entity.Request {
	return entity.Request{
		YoutubeURL: "https://www.youtube.com/watch?v=HDDdKCSn25Y",
		Start:      "00:10",
		End:        "00:20",
		Duration:   "01:00",
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func floatPointer(value float64) *float64 {
	return &value
}

func TestServicePassesCrossfadeToProcessor(t *testing.T) {
	proc := &fakeProcessor{}
	svc := newTestService(t, &fakeDownloader{}, proc)

	req := enabledRequest()
	req.Crossfade = floatPointer(1.25)

	result, err := svc.CreateLoopedAudio(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateLoopedAudio returned error: %v", err)
	}

	if !result.Seamless || result.Data == nil {
		t.Fatalf("unexpected result: %+v", result)
	}

	if !proc.seamless {
		t.Fatalf("expected the seamless processor path")
	}

	if proc.crossfade != 1250*time.Millisecond {
		t.Fatalf("crossfade = %s, want 1.25s", proc.crossfade)
	}
}

func TestServiceUsesPlainPathWhenSeamlessDisabled(t *testing.T) {
	proc := &fakeProcessor{}
	svc := newTestService(t, &fakeDownloader{}, proc)

	req := enabledRequest()
	req.Seamless = boolPointer(false)
	req.Crossfade = floatPointer(4)

	result, err := svc.CreateLoopedAudio(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateLoopedAudio returned error: %v", err)
	}

	if proc.seamless {
		t.Fatalf("expected the plain concatenation path")
	}

	if result.Seamless || result.Timing != nil || result.Crossfade != 0 {
		t.Fatalf("plain result should not carry seamless metadata: %+v", result)
	}
}

func TestServiceCleansJobDirectoryOnDownloadError(t *testing.T) {
	baseDir := t.TempDir()
	svc := newAudioService(
		&fakeDownloader{err: errors.New("yt-dlp failed")},
		&fakeProcessor{},
		NewJobManager(baseDir),
	)

	_, err := svc.CreateLoopedAudio(context.Background(), enabledRequest())
	if err == nil {
		t.Fatalf("expected a download error")
	}

	assertNoJobDirectories(t, baseDir)
}

func TestServiceCleansJobDirectoryOnProcessingError(t *testing.T) {
	baseDir := t.TempDir()
	proc := &fakeProcessor{err: processor.ErrNoAudioTrack}
	svc := newAudioService(&fakeDownloader{}, proc, NewJobManager(baseDir))

	_, err := svc.CreateLoopedAudio(context.Background(), enabledRequest())
	if !errors.Is(err, processor.ErrNoAudioTrack) {
		t.Fatalf("expected ErrNoAudioTrack, got %v", err)
	}

	assertNoJobDirectories(t, baseDir)
}

func TestServiceCleansJobDirectoryOnCanceledDownload(t *testing.T) {
	baseDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	downloader := &fakeDownloader{
		blockCh: make(chan struct{}),
		onStart: func(string) {
			cancel()
		},
	}

	svc := newAudioService(downloader, &fakeProcessor{}, NewJobManager(baseDir))

	result, err := svc.CreateLoopedAudio(ctx, enabledRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	if result != nil {
		t.Fatalf("expected no result on cancellation")
	}

	assertNoJobDirectories(t, baseDir)
}

func TestServiceCleansPartialOutputOnProcessingCancel(t *testing.T) {
	baseDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	proc := &fakeProcessor{
		err: context.Canceled,
		onSeamless: func(outputPath string) {
			// Emulate a partially written encode before cancellation.
			_ = os.WriteFile(outputPath, []byte("partial"), 0644)
			cancel()
		},
	}

	svc := newAudioService(&fakeDownloader{}, proc, NewJobManager(baseDir))

	_, err := svc.CreateLoopedAudio(ctx, enabledRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	assertNoJobDirectories(t, baseDir)
}

func TestServiceRejectsInvalidDuration(t *testing.T) {
	baseDir := t.TempDir()
	svc := newAudioService(&fakeDownloader{}, &fakeProcessor{}, NewJobManager(baseDir))

	req := enabledRequest()
	req.Duration = "bad"

	if _, err := svc.CreateLoopedAudio(context.Background(), req); err == nil {
		t.Fatalf("expected duration parsing error")
	}

	assertNoJobDirectories(t, baseDir)
}

func assertNoJobDirectories(t *testing.T, baseDir string) {
	t.Helper()

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		t.Fatalf("read job base dir: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			path := filepath.Join(baseDir, entry.Name())

			leftovers, _ := os.ReadDir(path)
			names := make([]string, 0, len(leftovers))
			for _, leftover := range leftovers {
				names = append(names, leftover.Name())
			}

			t.Fatalf("job directory %s was not cleaned up, contents: %v", path, names)
		}
	}
}
