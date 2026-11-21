package validator

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/cobrich/youtube-audio-looper/internal/entity"
)

func baseRequest() entity.Request {
	return entity.Request{
		YoutubeURL: "https://www.youtube.com/watch?v=HDDdKCSn25Y",
		Start:      "00:10",
		End:        "00:20",
		Duration:   "01:00",
	}
}

func floatPointer(value float64) *float64 {
	return &value
}

func boolPointer(value bool) *bool {
	return &value
}

func TestValidateDefaultsAreSeamless(t *testing.T) {
	req := baseRequest()

	if !req.SeamlessEnabled() {
		t.Fatalf("seamless should default to enabled")
	}

	if got := req.CrossfadeSeconds(); got != entity.DefaultCrossfadeSeconds {
		t.Fatalf("default crossfade = %v, want %v", got, entity.DefaultCrossfadeSeconds)
	}

	if err := ValidateCreateLoopedAudioRequest(req); err != nil {
		t.Fatalf("default request should validate, got %v", err)
	}
}

func TestValidateCrossfadeBounds(t *testing.T) {
	tests := []struct {
		name      string
		crossfade float64
		wantErr   bool
	}{
		{name: "zero", crossfade: 0},
		{name: "default", crossfade: 0.5},
		{name: "segment-bound maximum", crossfade: 5},
		{name: "negative", crossfade: -0.1, wantErr: true},
		{name: "above maximum", crossfade: MaxCrossfadeSeconds + 0.1, wantErr: true},
		{name: "NaN", crossfade: math.NaN(), wantErr: true},
		{name: "infinity", crossfade: math.Inf(1), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := baseRequest()
			req.Crossfade = floatPointer(tt.crossfade)

			err := ValidateCreateLoopedAudioRequest(req)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidCrossfade) {
					t.Fatalf("expected ErrInvalidCrossfade, got %v", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateCrossfadeIgnoredWhenSeamlessDisabled(t *testing.T) {
	req := baseRequest()
	req.Seamless = boolPointer(false)
	req.Crossfade = floatPointer(-5)

	if err := ValidateCreateLoopedAudioRequest(req); err != nil {
		t.Fatalf("crossfade should be ignored when seamless is off, got %v", err)
	}
}

func TestValidateLoopCountBounds(t *testing.T) {
	tests := []struct {
		name    string
		loops   int
		wantErr bool
	}{
		{name: "zero means derived", loops: 0},
		{name: "one", loops: 1},
		{name: "maximum", loops: MaxLoopCount},
		{name: "negative", loops: -1, wantErr: true},
		{name: "above maximum", loops: MaxLoopCount + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := baseRequest()
			req.Loops = intPointer(tt.loops)

			err := ValidateCreateLoopedAudioRequest(req)
			if tt.wantErr && err == nil {
				t.Fatalf("expected an error")
			}

			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func intPointer(value int) *int {
	return &value
}

func TestValidateRejectsCrossfadeLongerThanSegment(t *testing.T) {
	req := baseRequest()
	// Segment is 10 seconds; an 11-second crossfade cannot fit.
	req.Crossfade = floatPointer(11)

	if err := ValidateCreateLoopedAudioRequest(req); !errors.Is(err, ErrInvalidCrossfade) {
		t.Fatalf("expected ErrInvalidCrossfade, got %v", err)
	}

	// More than half of a 10-second segment would overlap more than two copies.
	req.Crossfade = floatPointer(5.1)

	if err := ValidateCreateLoopedAudioRequest(req); !errors.Is(err, ErrInvalidCrossfade) {
		t.Fatalf("expected ErrInvalidCrossfade for more than half, got %v", err)
	}

	// Exactly half is allowed.
	req.Crossfade = floatPointer(5)

	if err := ValidateCreateLoopedAudioRequest(req); err != nil {
		t.Fatalf("half-length crossfade should be allowed, got %v", err)
	}
}

func TestValidateKeepsOriginalRules(t *testing.T) {
	req := baseRequest()
	req.End = "00:05"

	if err := ValidateCreateLoopedAudioRequest(req); err == nil {
		t.Fatalf("end before start should fail")
	}

	req = baseRequest()
	req.Duration = "00:00"

	if err := ValidateCreateLoopedAudioRequest(req); err == nil {
		t.Fatalf("zero duration should fail")
	}

	req = baseRequest()
	req.YoutubeURL = "https://example.com/watch?v=1"

	if err := ValidateCreateLoopedAudioRequest(req); err == nil {
		t.Fatalf("non-YouTube URL should fail")
	}
}

func TestRequestJSONRoundTrip(t *testing.T) {
	var req entity.Request

	if err := json.Unmarshal([]byte(`{
		"youtube_url": "https://youtu.be/abc",
		"start": "00:00",
		"end": "00:05",
		"duration": "00:30",
		"crossfade": 1.25,
		"seamless": true
	}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !req.SeamlessEnabled() || req.CrossfadeSeconds() != 1.25 {
		t.Fatalf("unexpected parsed request: %+v", req)
	}

	var legacy entity.Request
	if err := json.Unmarshal([]byte(`{
		"youtube_url": "https://youtu.be/abc",
		"start": "00:00",
		"end": "00:05",
		"duration": "00:30"
	}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}

	if !legacy.SeamlessEnabled() || legacy.CrossfadeSeconds() != entity.DefaultCrossfadeSeconds {
		t.Fatalf("legacy request should keep seamless defaults: %+v", legacy)
	}

	body, err := json.Marshal(entity.Request{
		YoutubeURL: "https://youtu.be/abc",
		Start:      "00:00",
		End:        "00:05",
		Duration:   "00:30",
		Seamless:   boolPointer(false),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if string(body) != `{"youtube_url":"https://youtu.be/abc","start":"00:00","end":"00:05","duration":"00:30","seamless":false}` {
		t.Fatalf("unexpected JSON: %s", body)
	}
}
