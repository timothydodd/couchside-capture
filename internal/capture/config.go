// Package capture turns a web page into a live HLS stream, encoding only
// while someone is watching.
package capture

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config comes from CAPTURE_* environment variables.
type Config struct {
	URL      string        // the page to show (required)
	Addr     string        // listen address
	Width    int           // picture size
	Height   int           //
	FPS      int           // frames per second captured and encoded
	BitrateK int           // video bitrate, kbit/s
	Preset   string        // x264 preset: ultrafast … veryfast; lighter is less CPU for a bigger picture
	Quality  int           // JPEG quality of the frames Chromium hands over (1-100): lower is less work for it
	HWAccel  string        // "vaapi" encodes on an Intel/AMD GPU through /dev/dri; "" is software
	VAAPI    string        // the render node for vaapi
	Music    string        // folder of MP3/AAC/Ogg files looped under the picture; "" for silence
	Idle     time.Duration // stop encoding this long after the last request
	Warm     bool          // keep the page loaded (frozen) between viewers; false launches the browser on demand
	Reload   time.Duration // a page asleep longer than this is reloaded on wake, so its data is fresh
	Chrome   string        // Chromium executable; "" lets chromedp find one
	FFmpeg   string        // ffmpeg executable
	Dir      string        // where segments are written
	StartMax time.Duration // how long a playlist request waits for the first segment
}

func FromEnv() (Config, error) {
	c := Config{
		URL: os.Getenv("CAPTURE_URL"), Addr: envOr("CAPTURE_ADDR", ":9800"),
		Width: envInt("CAPTURE_WIDTH", 640), Height: envInt("CAPTURE_HEIGHT", 480), FPS: envInt("CAPTURE_FPS", 10),
		BitrateK: envInt("CAPTURE_BITRATE_K", 1500), Music: os.Getenv("CAPTURE_MUSIC"),
		Preset: envOr("CAPTURE_PRESET", "superfast"), Quality: envInt("CAPTURE_QUALITY", 75),
		HWAccel: envOr("CAPTURE_HWACCEL", ""), VAAPI: envOr("CAPTURE_VAAPI_DEVICE", "/dev/dri/renderD128"),
		Idle: envDur("CAPTURE_IDLE", time.Minute), Warm: envOr("CAPTURE_MODE", "warm") != "cold",
		Reload: envDur("CAPTURE_RELOAD_AFTER", 10*time.Minute), Chrome: os.Getenv("CAPTURE_CHROME"),
		FFmpeg: envOr("CAPTURE_FFMPEG", "ffmpeg"), Dir: envOr("CAPTURE_DIR", os.TempDir()+"/couchside-capture"),
		StartMax: envDur("CAPTURE_START_MAX", 25*time.Second),
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return c, fmt.Errorf("CAPTURE_URL must be the http(s) address of the page to show")
	}
	if c.Width < 160 || c.Height < 120 || c.Width > 3840 || c.Height > 2160 {
		return c, fmt.Errorf("CAPTURE_WIDTH and CAPTURE_HEIGHT must be between 160x120 and 3840x2160")
	}
	if c.FPS < 1 || c.FPS > 30 {
		return c, fmt.Errorf("CAPTURE_FPS must be 1 to 30")
	}
	switch c.Preset {
	case "ultrafast", "superfast", "veryfast", "faster", "fast", "medium":
	default:
		return c, fmt.Errorf("CAPTURE_PRESET is an x264 preset: ultrafast, superfast, veryfast, faster, fast or medium")
	}
	if c.Quality < 1 || c.Quality > 100 {
		return c, fmt.Errorf("CAPTURE_QUALITY is 1 to 100")
	}
	if c.HWAccel != "" && c.HWAccel != "vaapi" && c.HWAccel != "none" {
		return c, fmt.Errorf("CAPTURE_HWACCEL is vaapi or empty")
	}
	if c.HWAccel == "none" {
		c.HWAccel = ""
	}
	if m := envOr("CAPTURE_MODE", "warm"); m != "warm" && m != "cold" {
		return c, fmt.Errorf("CAPTURE_MODE is warm or cold")
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(envOr(k, "")); err == nil {
		return n
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(envOr(k, "")); err == nil && d > 0 {
		return d
	}
	return def
}
