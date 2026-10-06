package capture

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Server serves the stream and runs its lifecycle: the first request for the
// playlist starts the encoder (waking the page, or launching the browser in
// cold mode), every request keeps it alive, and cfg.Idle after the last one
// it stops and the page goes back to sleep.
type Server struct {
	cfg     Config
	browser *Browser

	mu       sync.Mutex
	enc      *Encoder
	last     time.Time // the last request for the stream
	starting chan struct{}
	startErr error
	started  time.Time
}

func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg, browser: NewBrowser(cfg)}
}

// Run prepares the page (warm mode) and reaps idle encoders until ctx ends.
func (s *Server) Run(ctx context.Context) {
	if s.cfg.Warm {
		if err := s.browser.Open(ctx); err != nil {
			slog.Error("opening the page; will try again when someone watches", "err", err)
		} else if err := s.browser.Freeze(); err != nil {
			slog.Warn("freezing the page", "err", err)
		}
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			if s.enc != nil {
				s.enc.Stop()
				s.enc = nil
			}
			s.mu.Unlock()
			s.browser.Close()
			return
		case <-t.C:
			s.reap()
		}
	}
}

// reap stops an encoder nobody has asked anything of for cfg.Idle, or one
// whose ffmpeg died.
func (s *Server) reap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enc == nil || s.starting != nil {
		return
	}
	idle := time.Since(s.last)
	if s.enc.Running() && idle < s.cfg.Idle {
		return
	}
	if !s.enc.Running() {
		slog.Warn("ffmpeg stopped on its own", "said", lastLine(s.enc.Error()))
	} else {
		slog.Info("nobody watching: stopping", "idle", idle.Round(time.Second), "ran", time.Since(s.started).Round(time.Second))
	}
	s.stopLocked()
}

func (s *Server) stopLocked() {
	_ = s.browser.StopCapture()
	s.enc.Stop()
	s.enc = nil
	if s.cfg.Warm {
		if err := s.browser.Freeze(); err != nil {
			slog.Warn("freezing the page", "err", err)
		}
	} else {
		s.browser.Close()
	}
}

// ensure has the encoder running, starting it if need be. Concurrent first
// requests share one start.
func (s *Server) ensure(ctx context.Context) (*Encoder, error) {
	s.mu.Lock()
	s.last = time.Now()
	if s.enc != nil && s.enc.Running() && s.starting == nil {
		e := s.enc
		s.mu.Unlock()
		return e, nil
	}
	if s.starting != nil {
		wait := s.starting
		s.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s.mu.Lock()
		e, err := s.enc, s.startErr
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return e, nil
	}
	if s.enc != nil { // died
		s.enc.Stop()
		s.enc = nil
	}
	s.starting = make(chan struct{})
	s.mu.Unlock()

	e, err := s.start(ctx)

	s.mu.Lock()
	s.enc, s.startErr, s.started = e, err, time.Now()
	close(s.starting)
	s.starting = nil
	s.mu.Unlock()
	return e, err
}

func (s *Server) start(ctx context.Context) (*Encoder, error) {
	t0 := time.Now()
	if !s.browser.Alive() {
		if err := s.browser.Open(ctx); err != nil {
			return nil, err
		}
	} else if err := s.browser.Wake(ctx); err != nil {
		return nil, err
	}
	if err := s.browser.StartCapture(); err != nil {
		return nil, err
	}
	e, err := StartEncoder(ctx, s.cfg, filepath.Join(s.cfg.Dir, "hls"), s.browser.Latest)
	if err != nil {
		_ = s.browser.StopCapture()
		return nil, err
	}
	slog.Info("someone's watching: encoding", "took", time.Since(t0).Round(time.Millisecond))
	return e, nil
}

var reSegment = regexp.MustCompile(`^seg\d+\.ts$`)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /"+playlistNm, s.playlist)
	mux.HandleFunc("GET /{seg}", s.segment)
	return mux
}

type status struct {
	State  string `json:"state"` // idle | starting | streaming
	Frames int64  `json:"frames,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	st := status{State: "idle"}
	switch {
	case s.starting != nil:
		st.State = "starting"
	case s.enc != nil && s.enc.Running():
		st.State, st.Frames = "streaming", s.browser.Frames()
	}
	if s.startErr != nil {
		st.Error = s.startErr.Error()
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// playlist starts the stream if need be and answers once the first segment
// exists (ffmpeg writes the playlist after it), within cfg.StartMax.
func (s *Server) playlist(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.StartMax)
	defer cancel()
	e, err := s.ensure(ctx)
	if err != nil {
		slog.Warn("can't start the stream", "err", err)
		http.Error(w, "the stream couldn't start: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	for {
		if _, err := os.Stat(e.Playlist()); err == nil {
			break
		}
		if !e.Running() {
			http.Error(w, "the encoder stopped: "+lastLine(e.Error()), http.StatusServiceUnavailable)
			return
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				http.Error(w, "the stream is still starting; try again", http.StatusServiceUnavailable)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, e.Playlist())
}

func (s *Server) segment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("seg")
	if !reSegment.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.last = time.Now()
	e := s.enc
	s.mu.Unlock()
	if e == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(filepath.Dir(e.Playlist()), name))
}

// Describe is a one-line summary for the log.
func (c Config) Describe() string {
	mode := "warm (page kept loaded, frozen between viewers)"
	if !c.Warm {
		mode = "cold (browser launched per viewer)"
	}
	music := "silence"
	if c.Music != "" {
		music = "music from " + c.Music
	}
	enc := "x264 " + c.Preset
	if c.HWAccel == "vaapi" {
		enc = "VAAPI on " + c.VAAPI
	}
	return strings.Join([]string{c.URL, mode, music, enc}, "; ")
}
