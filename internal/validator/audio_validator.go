package validator

import (
	"errors"
	"log"
	"math"
	"net/url"
	"strings"

	"github.com/cobrich/youtube-audio-looper/internal/entity"
	"github.com/cobrich/youtube-audio-looper/internal/utils"
)

const (
	MaxDurationSeconds   = 600
	MaxClipLengthSeconds = 60
	MinCrossfadeSeconds  = 0
	MaxCrossfadeSeconds  = 10
	MaxLoopCount         = 1000
)

// ErrInvalidCrossfade is returned for crossfade values that cannot be applied.
var ErrInvalidCrossfade = errors.New("invalid crossfade duration")

func ValidateCreateLoopedAudioRequest(req entity.Request) error {
	if !isValidURL(req.YoutubeURL) {
		return errors.New("invalid youtube url")
	}

	startSeconds, err := utils.ParseTimeToSeconds(req.Start)
	if err != nil {
		log.Println(err)

		return err
	}

	endSeconds, err := utils.ParseTimeToSeconds(req.End)
	if err != nil {
		log.Println(err)

		return err
	}

	if endSeconds <= startSeconds {
		log.Println(err)

		return errors.New("end time must be older than start")
	}

	durationSeconds, err := utils.ParseTimeToSeconds(req.Duration)
	if err != nil {
		log.Println(err)

		return err
	}

	if durationSeconds <= 0 {
		log.Println(err)

		return errors.New("duration must be grater than zero")
	}

	if endSeconds-startSeconds > durationSeconds {
		log.Println(err)

		return errors.New("selected segment cannot be longer than final duration")
	}

	if durationSeconds > MaxDurationSeconds {
		return errors.New("maximum duration is 10 minutes")
	}

	if endSeconds-startSeconds > MaxClipLengthSeconds {
		return errors.New("the clip length should be less than minute")
	}

	if err := validateCrossfade(req, endSeconds-startSeconds); err != nil {
		return err
	}

	if err := validateLoopCount(req); err != nil {
		return err
	}

	return nil
}

// validateCrossfade rejects non-finite or out-of-range crossfades, and fades
// that cannot fit inside the clip. The precise clip length is known only after
// decoding; this catches the obviously impossible cases early, and the
// processor still re-checks against the real decoded length.
func validateCrossfade(req entity.Request, clipSeconds int) error {
	if !req.SeamlessEnabled() {
		// Crossfade is ignored when seamless looping is off.
		return nil
	}

	crossfade := req.CrossfadeSeconds()

	if math.IsNaN(crossfade) || math.IsInf(crossfade, 0) {
		return ErrInvalidCrossfade
	}

	if crossfade < MinCrossfadeSeconds || crossfade > MaxCrossfadeSeconds {
		return ErrInvalidCrossfade
	}

	// The crossfade consumes the clip's head and tail, so it can use at most
	// half of the clip. The processor re-checks this against the actual
	// decoded length, because downloads can be slightly shorter than the
	// requested timestamps.
	if clipSeconds > 0 && crossfade*2 > float64(clipSeconds) {
		return ErrInvalidCrossfade
	}

	return nil
}

// validateLoopCount rejects explicit loop counts outside the supported range.
// Zero means "derive from duration".
func validateLoopCount(req entity.Request) error {
	if !req.SeamlessEnabled() {
		return nil
	}

	loops := req.LoopCount()
	if loops < 0 || loops > MaxLoopCount {
		return errors.New("loop count must be between 0 and 1000")
	}

	return nil
}

func isValidURL(rawURL string) bool {
	parsedURL, err := url.ParseRequestURI(rawURL)
	if err != nil {
		log.Println(err)

		return false
	}

	host := strings.ToLower(parsedURL.Host)

	return host == "youtube.com" ||
		host == "www.youtube.com" ||
		host == "youtu.be" ||
		host == "m.youtube.com" ||
		host == "music.youtube.com"
}
