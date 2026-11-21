package processor

import (
	"errors"
	"fmt"
	"math"
)

var (
	// ErrNoAudioTrack means the input container has no decodable audio stream.
	ErrNoAudioTrack = errors.New("input has no audio track")
	// ErrEmptyClip means the decoded audio contains no samples.
	ErrEmptyClip = errors.New("input audio is empty")
	// ErrUnsupportedChannels means the input has more channels than the MP3 encoder supports.
	ErrUnsupportedChannels = errors.New("input audio has unsupported channel count")
	// ErrInvalidAudio means the input could not be decoded as audio.
	ErrInvalidAudio = errors.New("input audio is invalid")
)

// MaxSupportedChannels is the largest channel count libmp3lame can encode.
const MaxSupportedChannels = 2

// PCMClip is a decoded audio clip stored as planar float32 samples.
// Channels are stored separately so stereo processing never mixes the left
// and right signals.
type PCMClip struct {
	SampleRate int
	Channels   [][]float32
}

// ChannelCount returns the number of audio channels.
func (c *PCMClip) ChannelCount() int {
	return len(c.Channels)
}

// SampleCount returns the number of samples in one channel.
func (c *PCMClip) SampleCount() int {
	if len(c.Channels) == 0 {
		return 0
	}

	return len(c.Channels[0])
}

// DurationSeconds returns the clip length derived from its sample count.
func (c *PCMClip) DurationSeconds() float64 {
	if c.SampleRate <= 0 {
		return 0
	}

	return float64(c.SampleCount()) / float64(c.SampleRate)
}

func (c *PCMClip) validate() error {
	if c == nil {
		return ErrEmptyClip
	}

	if c.SampleRate <= 0 {
		return fmt.Errorf("%w: invalid sample rate %d", ErrInvalidAudio, c.SampleRate)
	}

	if len(c.Channels) == 0 {
		return ErrNoAudioTrack
	}

	if len(c.Channels) > MaxSupportedChannels {
		return fmt.Errorf("%w: %d channels", ErrUnsupportedChannels, len(c.Channels))
	}

	samples := len(c.Channels[0])
	if samples == 0 {
		return ErrEmptyClip
	}

	for _, channel := range c.Channels {
		if len(channel) != samples {
			return fmt.Errorf("%w: channel sample counts differ", ErrInvalidAudio)
		}
	}

	return nil
}

func clampSample(value float64) float32 {
	if math.IsNaN(value) {
		return 0
	}

	if value > 1 {
		return 1
	}

	if value < -1 {
		return -1
	}

	return float32(value)
}
