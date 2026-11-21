package entity

const (
	// DefaultCrossfadeSeconds is used when a request enables seamless looping
	// but does not specify its own crossfade length.
	DefaultCrossfadeSeconds = 0.5
)

type Request struct {
	YoutubeURL string `json:"youtube_url" binding:"required"`
	Start      string `json:"start" binding:"required"`
	End        string `json:"end" binding:"required"`
	Duration   string `json:"duration" binding:"required"`

	// Crossfade is the seamless crossfade length in seconds. It is ignored
	// when Seamless is false. Omitted values use DefaultCrossfadeSeconds.
	Crossfade *float64 `json:"crossfade,omitempty"`
	// Seamless defaults to true. Set it to false to keep plain clip
	// concatenation.
	Seamless *bool `json:"seamless,omitempty"`
	// Loops overrides how many loop units are written. Zero or omitted derives
	// the count from Duration.
	Loops *int `json:"loops,omitempty"`
}

// SeamlessEnabled reports whether crossfaded looping should be used.
func (r Request) SeamlessEnabled() bool {
	return r.Seamless == nil || *r.Seamless
}

// CrossfadeSeconds returns the effective crossfade length in seconds.
func (r Request) CrossfadeSeconds() float64 {
	if r.Crossfade == nil {
		return DefaultCrossfadeSeconds
	}

	return *r.Crossfade
}

// LoopCount returns the explicit loop count, or zero when it should be derived
// from Duration.
func (r Request) LoopCount() int {
	if r.Loops == nil {
		return 0
	}

	return *r.Loops
}
