package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/cobrich/youtube-audio-looper/internal/downloader"
	"github.com/cobrich/youtube-audio-looper/internal/entity"
	"github.com/cobrich/youtube-audio-looper/internal/processor"
	"github.com/cobrich/youtube-audio-looper/internal/service"
	"github.com/cobrich/youtube-audio-looper/internal/validator"
	"github.com/gin-gonic/gin"
)

// AudioCreator is the service surface the handler needs.
type AudioCreator interface {
	CreateLoopedAudio(ctx context.Context, req entity.Request) (*service.Result, error)
}

type AudioHandler struct {
	audioService AudioCreator
}

func NewAudioHandler(audioService AudioCreator) *AudioHandler {
	return &AudioHandler{
		audioService: audioService,
	}
}

func (h *AudioHandler) CreateLoopedAudio(c *gin.Context) {
	var req entity.Request

	if err := c.ShouldBindJSON(&req); err != nil {
		log.Println(err)

		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid request body",
		})
		return
	}

	if err := validator.ValidateCreateLoopedAudioRequest(req); err != nil {
		log.Println(err)

		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}

	result, err := h.audioService.CreateLoopedAudio(c.Request.Context(), req)
	if err != nil {
		h.respondWithError(c, err)
		return
	}

	c.Header("Content-Disposition", `attachment; filename="looped-audio.mp3"`)
	c.Header("X-Seamless", boolHeader(result.Seamless))

	if result.Seamless {
		c.Header("X-Crossfade-Seconds", strconv.FormatFloat(result.Crossfade, 'f', 3, 64))
	}

	if result.Timing != nil {
		timing := result.Timing

		c.Header("X-Loop-Count", strconv.Itoa(timing.LoopCount))
		c.Header("X-Clip-Seconds", strconv.FormatFloat(
			float64(timing.ClipFrames)/float64(timing.SampleRate), 'f', 3, 64,
		))
		c.Header("X-Output-Seconds", strconv.FormatFloat(
			timing.OutputDurationSeconds(), 'f', 3, 64,
		))
	}

	c.Data(
		http.StatusOK,
		"audio/mpeg",
		result.Data,
	)
}

func boolHeader(value bool) string {
	if value {
		return "true"
	}

	return "false"
}

func (h *AudioHandler) respondWithError(c *gin.Context, err error) {
	log.Println("create audio error:", err)

	switch {
	case errors.Is(err, context.Canceled):
		// The client went away; there is nobody left to send a body to.
		log.Println("request canceled by client")

		c.AbortWithStatus(http.StatusRequestTimeout)
	case errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusGatewayTimeout, gin.H{
			"error": "audio processing timed out",
		})
	case errors.Is(err, downloader.ErrAuthenticationRequired):
		c.JSON(http.StatusBadGateway, gin.H{
			"error": "YouTube requires authentication for this video. Configure server cookies and try again.",
		})
	case errors.Is(err, downloader.ErrCookieFileUnavailable):
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Server cookie file is not available. Check YTDLP_COOKIES_PATH configuration.",
		})
	case errors.Is(err, processor.ErrNoAudioTrack):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "Downloaded segment has no audio track",
		})
	case errors.Is(err, processor.ErrEmptyClip):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "Downloaded segment contains no audio samples",
		})
	case errors.Is(err, processor.ErrCrossfadeTooLong):
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Crossfade is too long for the downloaded segment. Use a shorter crossfade or a longer segment.",
		})
	case errors.Is(err, processor.ErrUnsupportedChannels), errors.Is(err, processor.ErrInvalidAudio):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "Downloaded segment could not be decoded as audio",
		})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create audio"})
	}
}
