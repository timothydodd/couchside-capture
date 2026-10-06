package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("CAPTURE_URL", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("no URL: want an error")
	}
	t.Setenv("CAPTURE_URL", "http://ws4kp:8080/?kiosk=true")
	t.Setenv("CAPTURE_FPS", "60")
	if _, err := FromEnv(); err == nil {
		t.Fatal("60 fps: want an error")
	}
	t.Setenv("CAPTURE_FPS", "")
	t.Setenv("CAPTURE_MODE", "cold")
	t.Setenv("CAPTURE_IDLE", "90s")
	c, err := FromEnv()
	if err != nil || c.Warm || c.Idle != 90*time.Second || c.FPS != 10 || c.Width != 640 {
		t.Fatalf("config = %+v, %v", c, err)
	}
}

// The ffmpeg command: frames on stdin, music when there's a folder, silence
// otherwise, HLS out.
func TestEncoderArgs(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{FPS: 10, Width: 641, Height: 480, BitrateK: 1500}
	args, err := encoderArgs(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(args, " ")
	for _, want := range []string{"-f image2pipe -framerate 10 -c:v mjpeg -i pipe:0", "anullsrc", "scale=640:480:flags=bicubic:force_original_aspect_ratio=decrease,pad=640:480:-1:-1,setsar=1", "-g 20", "-hls_time 2", filepath.Join(dir, "stream.m3u8")} {
		if !strings.Contains(s, want) {
			t.Errorf("args lack %q:\n%s", want, s)
		}
	}
	music := t.TempDir()
	if _, err := encoderArgs(Config{FPS: 10, Width: 640, Height: 480, BitrateK: 1500, Music: music}, dir); err == nil {
		t.Fatal("an empty music folder: want an error")
	}
	_ = os.WriteFile(filepath.Join(music, "it's jazz.mp3"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(music, "notes.txt"), []byte("x"), 0o644)
	args, err = encoderArgs(Config{FPS: 10, Width: 640, Height: 480, BitrateK: 1500, Music: music}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "-stream_loop -1 -f concat") {
		t.Fatalf("music args: %v", args)
	}
	list, _ := os.ReadFile(filepath.Join(dir, "music.txt"))
	if got := strings.TrimSpace(string(list)); got != `file '`+filepath.Join(music, `it'\''s jazz.mp3`)+`'` {
		t.Fatalf("list = %s", got)
	}
}

// Stopping a process tree pauses the children too, and resuming brings them back.
func TestStopAndResumeTree(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & sleep 30 & wait")
	if err := cmd.Start(); err != nil {
		t.Skip("no sh:", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	time.Sleep(200 * time.Millisecond)
	tree := processTree(cmd.Process.Pid)
	if len(tree) < 3 {
		t.Fatalf("tree = %v, want the shell and two sleeps", tree)
	}
	state := func(pid int) byte {
		b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		s := string(b)
		return s[strings.LastIndex(s, ")")+2]
	}
	if err := stopTree(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // signals land asynchronously
	for _, p := range tree {
		if st := state(p); st != 'T' && st != 't' {
			t.Errorf("pid %d is %c after stop, want T", p, st)
		}
	}
	if err := contTree(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	for _, p := range tree {
		if st := state(p); st == 'T' {
			t.Errorf("pid %d still stopped after resume", p)
		}
	}
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
}
