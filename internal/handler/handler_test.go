package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cobrich/youtube-audio-looper/internal/entity"
	"github.com/cobrich/youtube-audio-looper/internal/processor"
	"github.com/cobrich/youtube-audio-looper/internal/service"
	"github.com/gin-gonic/gin"
)

type stubService struct {
	result *service.Result
	err    error
	gotReq entity.Request
}

func (s *stubService) CreateLoopedAudio(ctx context.Context, req entity.Request) (*service.Result, error) {
	s.gotReq = req

	return s.result, s.err
}

func newTestRouter(stub *stubService) *gin.Engine {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	handler := NewAudioHandler(stub)

	router.POST("/api/v1/audio/loop", handler.CreateLoopedAudio)

	return router
}

func TestHandlerErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "canceled", err: context.Canceled, wantCode: http.StatusRequestTimeout},
		{name: "deadline", err: context.DeadlineExceeded, wantCode: http.StatusGatewayTimeout},
		{name: "no audio track", err: processor.ErrNoAudioTrack, wantCode: http.StatusUnprocessableEntity},
		{name: "empty clip", err: processor.ErrEmptyClip, wantCode: http.StatusUnprocessableEntity},
		{name: "crossfade too long", err: processor.ErrCrossfadeTooLong, wantCode: http.StatusBadRequest},
		{name: "invalid audio", err: processor.ErrInvalidAudio, wantCode: http.StatusUnprocessableEntity},
		{name: "unknown", err: errors.New("boom"), wantCode: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newTestRouter(&stubService{err: tt.err})

			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/audio/loop",
				strings.NewReader(`{"youtube_url":"https://youtu.be/x","start":"00:00","end":"00:05","duration":"00:30"}`),
			)
			request.Header.Set("Content-Type", "application/json")

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d, body = %s", recorder.Code, tt.wantCode, recorder.Body.String())
			}
		})
	}
}

func TestHandlerPassesSeamlessParamsThrough(t *testing.T) {
	stub := &stubService{result: &service.Result{
		Data:     []byte("mp3"),
		Seamless: true,
		Timing:   &processor.LoopTiming{SampleRate: 44100, LoopCount: 7},
	}}
	router := newTestRouter(stub)

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/audio/loop",
		strings.NewReader(`{
			"youtube_url":"https://youtu.be/x",
			"start":"00:00",
			"end":"00:05",
			"duration":"00:30",
			"crossfade":0.75,
			"loops":7
		}`),
	)
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	if stub.gotReq.CrossfadeSeconds() != 0.75 || stub.gotReq.LoopCount() != 7 {
		t.Fatalf("handler did not pass through crossfade/loops: %+v", stub.gotReq)
	}

	if recorder.Header().Get("X-Seamless") != "true" {
		t.Fatalf("missing X-Seamless header")
	}

	if recorder.Header().Get("Content-Disposition") == "" {
		t.Fatalf("missing Content-Disposition header")
	}
}

func TestHandlerRejectsInvalidBody(t *testing.T) {
	router := newTestRouter(&stubService{})

	request := httptest.NewRequest(http.MethodPost, "/api/v1/audio/loop", strings.NewReader(`{`))
	request.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body["error"] != "invalid request body" {
		t.Fatalf("unexpected error body: %v", body)
	}
}
