package capture

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	segDur     = 2  // seconds per HLS segment
	listSize   = 10 // segments kept in the playlist (20s of rewind for a late joiner)
	playlistNm = "stream.m3u8"
)

// Encoder is one ffmpeg turning frames into HLS. Frames are pushed into its
// stdin at a steady rate, the newest one repeated when the page hasn't
// repainted, so the stream keeps time even on a still screen. Music loops
// underneath, or silence: players expect an audio track.
type Encoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	dir    string
	exited chan struct{}
	stderr bytes.Buffer
	errMu  sync.Mutex
}

// StartEncoder runs ffmpeg writing into dir, fed by frames (nil until the
// page has painted once).
func StartEncoder(ctx context.Context, cfg Config, dir string, frames func() []byte) (*Encoder, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	args, err := encoderArgs(cfg, dir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.FFmpeg, args...)
	e := &Encoder{cmd: cmd, dir: dir, exited: make(chan struct{})}
	cmd.Stderr = lockedWriter{&e.stderr, &e.errMu}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	e.stdin = stdin
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	go func() {
		_ = cmd.Wait()
		close(e.exited)
	}()
	go e.feed(cfg.FPS, frames)
	return e, nil
}

// feed writes a frame every 1/fps seconds until ffmpeg goes away.
func (e *Encoder) feed(fps int, frames func() []byte) {
	t := time.NewTicker(time.Second / time.Duration(fps))
	defer t.Stop()
	defer e.stdin.Close()
	for {
		select {
		case <-e.exited:
			return
		case <-t.C:
		}
		f := frames()
		if f == nil {
			continue
		}
		if _, err := e.stdin.Write(f); err != nil {
			return
		}
	}
}

// encoderArgs is the ffmpeg command: MJPEG frames on stdin, music or silence,
// H.264 and AAC into a sliding HLS playlist.
func encoderArgs(cfg Config, dir string) ([]string, error) {
	fps := strconv.Itoa(cfg.FPS)
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error",
		// Two frames are enough to see the input is MJPEG; probing a megabyte of
		// them (the default) delayed the first segment by seconds.
		"-analyzeduration", "0", "-probesize", "65536",
		"-f", "image2pipe", "-framerate", fps, "-c:v", "mjpeg", "-i", "pipe:0"}
	if cfg.Music != "" {
		list, err := musicList(cfg.Music, dir)
		if err != nil {
			return nil, err
		}
		// -re paces the music at real time; the frames arrive at real time already.
		args = append(args, "-re", "-stream_loop", "-1", "-f", "concat", "-safe", "0", "-i", list)
	} else {
		args = append(args, "-re", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo")
	}
	gop := strconv.Itoa(cfg.FPS * segDur)
	kbps := strconv.Itoa(cfg.BitrateK)
	// Fit the frame in the picture without distorting it (black bars if the
	// shape is off), and say the pixels are square: scale alone would keep
	// the frame's shape by flagging stretched pixels instead.
	vf := fmt.Sprintf("scale=%d:%d:flags=bicubic:force_original_aspect_ratio=decrease,pad=%d:%d:-1:-1,setsar=1",
		cfg.Width&^1, cfg.Height&^1, cfg.Width&^1, cfg.Height&^1)
	var video []string
	if cfg.HWAccel == "vaapi" {
		args = append([]string{args[0], args[1], args[2], args[3], "-vaapi_device", cfg.VAAPI}, args[4:]...)
		vf += ",format=nv12,hwupload"
		video = []string{"-c:v", "h264_vaapi", "-profile:v", "main", "-bf", "0"}
	} else {
		vf += ",format=yuv420p"
		video = []string{"-c:v", "libx264", "-preset", cfg.Preset, "-tune", "zerolatency", "-profile:v", "main"}
	}
	args = append(args, "-map", "0:v:0", "-map", "1:a:0", "-shortest") // closing the frames ends it; the audio never would
	args = append(args, "-vf", vf)
	args = append(args, video...)
	args = append(args,
		"-g", gop, "-keyint_min", gop, "-sc_threshold", "0", "-r", fps,
		"-b:v", kbps+"k", "-maxrate", kbps+"k", "-bufsize", strconv.Itoa(cfg.BitrateK*2)+"k",
		"-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2",
		"-f", "hls", "-hls_time", strconv.Itoa(segDur), "-hls_list_size", strconv.Itoa(listSize),
		"-hls_flags", "delete_segments+independent_segments+temp_file",
		"-hls_segment_filename", filepath.Join(dir, "seg%d.ts"), filepath.Join(dir, playlistNm))
	return args, nil
}

// TestHWAccel checks that the GPU encoder works (driver present, device
// reachable) with a one-frame encode, so a misconfigured GPU falls back to
// software at start rather than failing every stream.
func TestHWAccel(cfg Config) error {
	if cfg.HWAccel != "vaapi" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cfg.FFmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-vaapi_device", cfg.VAAPI,
		"-f", "lavfi", "-i", "color=black:s=64x64:r=10:d=0.2", "-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi", "-f", "null", "-").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, lastLine(string(out)))
	}
	return nil
}

// musicList writes a concat list of the folder's audio files, shuffled.
func musicList(folder, dir string) (string, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return "", fmt.Errorf("music folder: %w", err)
	}
	var files []string
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".mp3", ".m4a", ".aac", ".ogg", ".opus", ".flac", ".wav":
			files = append(files, filepath.Join(folder, e.Name()))
		}
	}
	if len(files) == 0 {
		return "", fmt.Errorf("music folder %s has no audio files", folder)
	}
	rand.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "file '%s'\n", strings.ReplaceAll(f, "'", `'\''`))
	}
	list := filepath.Join(dir, "music.txt")
	return list, os.WriteFile(list, []byte(b.String()), 0o644)
}

// Playlist is the playlist's path.
func (e *Encoder) Playlist() string { return filepath.Join(e.dir, playlistNm) }

// Running reports whether ffmpeg is still going.
func (e *Encoder) Running() bool {
	select {
	case <-e.exited:
		return false
	default:
		return true
	}
}

// Stop ends ffmpeg (closing its input lets it finish the segment it's on)
// and removes the segments.
func (e *Encoder) Stop() {
	_ = e.stdin.Close()
	select {
	case <-e.exited:
	case <-time.After(time.Second): // the segments are deleted anyway
		_ = e.cmd.Process.Kill()
		<-e.exited
	}
	if msg := strings.TrimSpace(e.Error()); msg != "" {
		slog.Warn("ffmpeg", "said", lastLine(msg))
	}
	_ = os.RemoveAll(e.dir)
}

// Error is what ffmpeg wrote to stderr so far.
func (e *Encoder) Error() string {
	e.errMu.Lock()
	defer e.errMu.Unlock()
	return e.stderr.String()
}

type lockedWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
