package capture

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image/jpeg"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Browser is one headless Chromium with the page open in a tab. Between
// viewers the page is frozen (Chromium's own background-tab state: no
// timers, no JavaScript, no painting), so it costs memory but no CPU, and
// wakes in well under a second. Frames come from the DevTools screencast,
// which hands over a JPEG of the tab whenever it repaints.
type Browser struct {
	cfg Config

	mu       sync.Mutex
	cancel   context.CancelFunc // the allocator: closes Chromium
	tab      context.Context
	frozen   bool
	sleptAt  time.Time
	captures int // StartCapture calls outstanding (one: the encoder)

	latest atomic.Pointer[[]byte] // the newest frame
	frames atomic.Int64           // frames received since StartCapture
}

func NewBrowser(cfg Config) *Browser { return &Browser{cfg: cfg} }

// Open launches Chromium and loads the page.
func (b *Browser) Open(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tab != nil && b.tab.Err() == nil {
		return nil
	}
	b.closeLocked()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.NoSandbox, // containers have no user namespaces for the sandbox
		chromedp.DisableGPU,
		chromedp.WindowSize(b.cfg.Width, b.cfg.Height),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		// No crash reporting: in a container there's nowhere for it to go, and
		// Alpine's Chromium refuses to start when its handler has no database.
		chromedp.Flag("disable-crash-reporter", true),
		chromedp.Flag("disable-crashpad", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("disable-dev-shm-usage", false), // /dev/shm is mounted large enough
	)
	if b.cfg.Chrome != "" {
		opts = append(opts, chromedp.ExecPath(b.cfg.Chrome))
	}
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	tab, cancelTab := chromedp.NewContext(allocCtx, chromedp.WithErrorf(func(format string, a ...any) {
		if !strings.Contains(format, "could not unmarshal event") {
			slog.Warn(fmt.Sprintf(format, a...))
		}
	}))
	all := func() { cancelTab(); cancel() }
	chromedp.ListenTarget(tab, b.onEvent(tab))
	loadCtx, done := context.WithTimeout(ctx, 60*time.Second)
	defer done()
	err := chromedp.Run(tab,
		emulation.SetDeviceMetricsOverride(int64(b.cfg.Width), int64(b.cfg.Height), 1, false),
		chromedp.Navigate(b.cfg.URL),
		chromedp.WaitReady("body"),
	)
	if err == nil {
		err = waitCtx(loadCtx, tab)
	}
	if err == nil {
		err = b.fitViewport(tab)
	}
	if err == nil {
		err = b.fitFrames(tab)
	}
	if err != nil {
		all()
		return fmt.Errorf("open %s: %w", b.cfg.URL, err)
	}
	b.cancel, b.tab, b.frozen = all, tab, false
	slog.Info("page open", "url", b.cfg.URL, "size", fmt.Sprintf("%dx%d", b.cfg.Width, b.cfg.Height))
	return nil
}

// fitViewport makes the page's viewport exactly the picture size. Some
// Chromium builds (Alpine's, in headless mode) count window decoration in
// --window-size, leaving the page shorter than asked, and the screencast
// then hands over frames of the wrong shape.
func (b *Browser) fitViewport(tab context.Context) error {
	measure := func() (w, h int, err error) {
		var dims []int
		if err := chromedp.Run(tab, chromedp.Evaluate(`[window.innerWidth, window.innerHeight]`, &dims)); err != nil {
			return 0, 0, err
		}
		if len(dims) != 2 {
			return 0, 0, errors.New("couldn't measure the viewport")
		}
		return dims[0], dims[1], nil
	}
	w, h, err := measure()
	if err != nil {
		return err
	}
	if w == b.cfg.Width && h == b.cfg.Height {
		return nil
	}
	// Grow (or shrink) the window by the difference, then look again.
	err = chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
		id, bounds, err := browser.GetWindowForTarget().Do(ctx)
		if err != nil {
			return err
		}
		return browser.SetWindowBounds(id, &browser.Bounds{
			Width:  bounds.Width + int64(b.cfg.Width-w),
			Height: bounds.Height + int64(b.cfg.Height-h),
		}).Do(ctx)
	}))
	if err != nil {
		return fmt.Errorf("resize window: %w", err)
	}
	time.Sleep(200 * time.Millisecond)
	w2, h2, err := measure()
	if err != nil {
		return err
	}
	if w2 != b.cfg.Width || h2 != b.cfg.Height {
		slog.Warn("the viewport isn't the picture size; frames will be letterboxed", "viewport", fmt.Sprintf("%dx%d", w2, h2),
			"wanted", fmt.Sprintf("%dx%d", b.cfg.Width, b.cfg.Height))
	} else {
		slog.Info("viewport corrected", "was", fmt.Sprintf("%dx%d", w, h), "now", fmt.Sprintf("%dx%d", w2, h2))
	}
	return nil
}

// fitFrames checks the size of the frames the screencast actually delivers.
// Some builds hand over only the part of the page inside the window's own
// bounds, which --window-size didn't make tall enough, so the frame is
// shorter than the viewport. The window is grown by the difference until a
// frame is the picture size.
func (b *Browser) fitFrames(tab context.Context) error {
	for attempt := 0; attempt < 3; attempt++ {
		w, h, err := b.sampleFrame(tab)
		if err != nil {
			slog.Warn("couldn't check the frame size", "err", err)
			return nil
		}
		if w == b.cfg.Width && h == b.cfg.Height {
			if attempt > 0 {
				slog.Info("frames now the picture size", "size", fmt.Sprintf("%dx%d", w, h))
			}
			return nil
		}
		slog.Info("frames aren't the picture size; growing the window", "frame", fmt.Sprintf("%dx%d", w, h),
			"wanted", fmt.Sprintf("%dx%d", b.cfg.Width, b.cfg.Height))
		err = chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
			id, bounds, err := browser.GetWindowForTarget().Do(ctx)
			if err != nil {
				return err
			}
			return browser.SetWindowBounds(id, &browser.Bounds{
				Width:  bounds.Width + int64(b.cfg.Width-w),
				Height: bounds.Height + int64(b.cfg.Height-h),
			}).Do(ctx)
		}))
		if err != nil {
			return fmt.Errorf("resize window: %w", err)
		}
		// The window grew, so the viewport may have too: pin it back.
		if err := chromedp.Run(tab, emulation.SetDeviceMetricsOverride(int64(b.cfg.Width), int64(b.cfg.Height), 1, false)); err != nil {
			return err
		}
		time.Sleep(300 * time.Millisecond)
	}
	slog.Warn("frames stay off the picture size; they'll be letterboxed")
	return nil
}

// sampleFrame runs the screencast just long enough to get one frame and
// reports its size.
func (b *Browser) sampleFrame(tab context.Context) (w, h int, err error) {
	b.latest.Store(nil)
	if err := chromedp.Run(tab, page.StartScreencast().WithFormat(page.ScreencastFormatJpeg).WithQuality(50).
		WithMaxWidth(int64(b.cfg.Width)).WithMaxHeight(int64(b.cfg.Height)).WithEveryNthFrame(1)); err != nil {
		return 0, 0, err
	}
	defer func() { _ = chromedp.Run(tab, page.StopScreencast()) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f := b.latest.Load(); f != nil {
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(*f))
			if err != nil {
				return 0, 0, err
			}
			return cfg.Width, cfg.Height, nil
		}
		// A still page paints nothing: nudge it.
		_ = chromedp.Run(tab, chromedp.Evaluate(`window.dispatchEvent(new Event('resize'))`, nil))
		time.Sleep(100 * time.Millisecond)
	}
	return 0, 0, errors.New("no frame arrived")
}

// waitCtx lets a load that chromedp.Run finished early still respect ctx.
func waitCtx(ctx, tab context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return tab.Err()
	}
}

func (b *Browser) onEvent(tab context.Context) func(any) {
	return func(ev any) {
		f, ok := ev.(*page.EventScreencastFrame)
		if !ok {
			return
		}
		data, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			return
		}
		b.latest.Store(&data)
		b.frames.Add(1)
		// Chromium sends the next frame only once this one is acknowledged.
		go func() { _ = chromedp.Run(tab, page.ScreencastFrameAck(f.SessionID)) }()
	}
}

// Close quits Chromium.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked()
}

func (b *Browser) closeLocked() {
	if b.frozen {
		if pid, err := b.pidLocked(); err == nil {
			_ = contTree(pid)
		}
		b.frozen = false
	}
	if b.cancel != nil {
		b.cancel()
	}
	b.cancel, b.tab = nil, nil
	b.latest.Store(nil)
}

// Freeze puts the page to sleep: no scripts, timers or painting until Wake.
func (b *Browser) Freeze() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tab == nil || b.frozen {
		return nil
	}
	pid, err := b.pidLocked()
	if err != nil {
		return err
	}
	if err := stopTree(pid); err != nil {
		return err
	}
	b.frozen, b.sleptAt = true, time.Now()
	slog.Info("browser frozen")
	return nil
}

// Wake resumes a frozen page. One asleep longer than cfg.Reload is reloaded
// first, so it shows current data rather than catching up its timers.
func (b *Browser) Wake(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tab == nil {
		return errors.New("the browser isn't open")
	}
	if !b.frozen {
		return nil
	}
	pid, err := b.pidLocked()
	if err != nil {
		return err
	}
	if err := contTree(pid); err != nil {
		return err
	}
	b.frozen = false
	if asleep := time.Since(b.sleptAt); asleep > b.cfg.Reload {
		slog.Info("page was asleep a while: reloading", "asleep", asleep.Round(time.Second))
		loadCtx, done := context.WithTimeout(ctx, 60*time.Second)
		defer done()
		if err := chromedp.Run(b.tab, chromedp.Reload(), chromedp.WaitReady("body")); err != nil {
			return fmt.Errorf("reload: %w", err)
		}
		if err := waitCtx(loadCtx, b.tab); err != nil {
			return err
		}
	}
	return nil
}

// pidLocked is Chromium's process id.
func (b *Browser) pidLocked() (int, error) {
	c := chromedp.FromContext(b.tab)
	if c == nil || c.Browser == nil || c.Browser.Process() == nil {
		return 0, errors.New("the browser has no process")
	}
	return c.Browser.Process().Pid, nil
}

// StartCapture asks Chromium for a JPEG of the tab on every repaint.
func (b *Browser) StartCapture() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tab == nil {
		return errors.New("the browser isn't open")
	}
	b.frames.Store(0)
	b.latest.Store(nil)
	return chromedp.Run(b.tab, page.StartScreencast().
		WithFormat(page.ScreencastFormatJpeg).WithQuality(85).
		WithMaxWidth(int64(b.cfg.Width)).WithMaxHeight(int64(b.cfg.Height)).WithEveryNthFrame(1))
}

func (b *Browser) StopCapture() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tab == nil {
		return nil
	}
	return chromedp.Run(b.tab, page.StopScreencast())
}

// Latest is the newest frame, nil before the first.
func (b *Browser) Latest() []byte {
	if p := b.latest.Load(); p != nil {
		return *p
	}
	return nil
}

// Frames is how many frames Chromium has sent since StartCapture.
func (b *Browser) Frames() int64 { return b.frames.Load() }

// Alive reports whether Chromium is still running.
func (b *Browser) Alive() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tab != nil && b.tab.Err() == nil
}
