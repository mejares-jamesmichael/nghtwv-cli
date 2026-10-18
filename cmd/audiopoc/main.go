// Command audiopoc is the Stage 1 harness that proves the audio output
// backends (PulseAudio protocol on Linux, WASAPI on Windows) before they
// replace mpv. It decodes the live Nightwave Plaza stream with Beep and
// writes the samples through the audio package.
//
// It is a development tool, not part of the shipped CLI.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/mp3"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/audio"
)

const defaultURL = "https://radio.plaza.one/mp3"

// options collects the harness flags.
type options struct {
	list     bool
	device   string
	secs     float64
	volume   float64
	tone     bool
	engine   bool
	spectrum bool
	pauseAt  float64
	pauseFor float64
	url      string
	rate     int
}

func main() {
	var opts options
	flag.BoolVar(&opts.list, "list", false, "list output devices and exit")
	flag.StringVar(&opts.device, "device", "", "stable device name to play through (empty = backend default)")
	flag.Float64Var(&opts.secs, "secs", 10, "seconds to play")
	flag.Float64Var(&opts.volume, "volume", 0.6, "software gain, 0..1")
	flag.BoolVar(&opts.tone, "tone", false, "play an 880Hz beep pattern instead of the stream")
	flag.BoolVar(&opts.engine, "engine", false, "run the decode engine (reconnect, volume, spectrum) instead of the raw loop")
	flag.BoolVar(&opts.spectrum, "spectrum", false, "print live band levels while -engine runs")
	flag.Float64Var(&opts.pauseAt, "pause-at", 0, "seconds into an -engine run to pause (0 disables)")
	flag.Float64Var(&opts.pauseFor, "pause-for", 2, "seconds to stay paused during -pause-at")
	flag.StringVar(&opts.url, "url", defaultURL, "stream URL")
	flag.IntVar(&opts.rate, "rate", 44100, "output sample rate")
	flag.Parse()
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "audiopoc:", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	out, err := audio.NewOutput(opts.rate)
	if err != nil {
		return err
	}
	defer out.Close()

	devices, err := out.Devices()
	if err != nil {
		return err
	}
	def, defErr := out.Default()
	fmt.Printf("devices (%d):\n", len(devices))
	for _, d := range devices {
		mark := " "
		if defErr == nil && d.Name == def.Name {
			mark = "*"
		}
		fmt.Printf("  %s %s — %s\n", mark, d.Name, d.Description)
	}
	if defErr != nil {
		fmt.Printf("(default device unavailable: %v)\n", defErr)
	}
	if opts.list {
		return nil
	}

	chosen := opts.device
	if chosen == "" {
		if defErr != nil {
			return fmt.Errorf("no default device: %w", defErr)
		}
		chosen = def.Name + " (default)"
	}

	if opts.engine {
		fmt.Printf("\nengine: %s for %.0fs through %s at volume %d\n", opts.url, opts.secs, chosen, int(opts.volume*100))
		return runEngine(out, opts)
	}

	if opts.tone {
		fmt.Printf("\nplaying a three-beep pattern for %.0fs through %s at gain %.2f\n", opts.secs, chosen, opts.volume)
		return playTone(out, opts.device, opts.rate, opts.secs, opts.volume)
	}

	fmt.Printf("\nplaying %s for %.0fs through %s at gain %.2f\n", opts.url, opts.secs, chosen, opts.volume)

	body, stream, format, err := openStream(opts.url)
	if err != nil {
		return err
	}
	defer body.Close()
	defer stream.Close()
	fmt.Printf("stream: %d Hz, %d channel(s)\n", format.SampleRate, format.NumChannels)

	if format.NumChannels != 2 {
		return fmt.Errorf("harness expects stereo, stream reports %d channel(s)", format.NumChannels)
	}
	var source beep.Streamer = stream
	if int(format.SampleRate) != opts.rate {
		source = beep.Resample(4, format.SampleRate, beep.SampleRate(opts.rate), source)
	}

	// Open after the decoder is ready so playback starts with data.
	if err := out.Open(opts.device); err != nil {
		return err
	}

	buf := make([][2]float64, 1024)
	f32 := make([]float32, 0, 2048)
	deadline := time.Now().Add(time.Duration(opts.secs * float64(time.Second)))
	written := 0
	for time.Now().Before(deadline) {
		n, ok := source.Stream(buf)
		if n > 0 {
			f32 = f32[:0]
			for _, sample := range buf[:n] {
				f32 = append(f32, gain(sample[0]*opts.volume), gain(sample[1]*opts.volume))
			}
			if err := out.Write(f32); err != nil {
				if reporter, ok := out.(audio.ErrReporter); ok && reporter.Err() != nil {
					return fmt.Errorf("output failed: %w", reporter.Err())
				}
				return fmt.Errorf("write: %w", err)
			}
			written += n
		}
		if !ok {
			fmt.Printf("stream drained after %d frames: %v\n", written, source.Err())
			break
		}
	}

	fmt.Printf("wrote %d frames (%.1fs), underflow=%v\n", written, float64(written)/float64(opts.rate), out.Underflow())
	if reporter, ok := out.(audio.ErrReporter); ok && reporter.Err() != nil {
		return reporter.Err()
	}
	return nil
}

// runEngine drives the real decode engine: reconnect handling, software
// volume, the PCM tap and the spectrum analyzer all live in the audio
// package, unlike this harness's raw loop above.
func runEngine(out audio.Output, opts options) error {
	if err := out.Open(opts.device); err != nil {
		return err
	}
	engine := audio.NewEngine(out, opts.rate, audio.PlazaOpener(opts.url), audio.WithVolume(int(opts.volume*100)))
	engine.Play()
	defer engine.Stop()

	start := time.Now()
	deadline := start.Add(time.Duration(opts.secs * float64(time.Second)))
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	paused, resumed := false, false
	for time.Now().Before(deadline) {
		<-ticker.C
		elapsed := time.Since(start).Seconds()
		if opts.pauseAt > 0 && !paused && elapsed >= opts.pauseAt {
			engine.Pause()
			paused = true
			fmt.Println("--- pause ---")
		}
		if paused && !resumed && elapsed >= opts.pauseAt+opts.pauseFor {
			engine.Play()
			resumed = true
			fmt.Println("--- resume ---")
		}
		status := engine.Status()
		snapshot := engine.Spectrum()
		if opts.spectrum {
			fmt.Printf("%-9s %s level=%.2f\n", status.State, bars(snapshot.Bands), snapshot.Level)
		} else {
			fmt.Printf("%-9s volume=%d level=%.2f\n", status.State, status.Volume, snapshot.Level)
		}
	}
	during := out.Underflow()
	engine.Stop()

	fmt.Printf("final: state=%v underflow during playback=%v (after stop=%v)",
		engine.Status().State, during, out.Underflow())
	if err := engine.Err(); err != nil {
		fmt.Printf(" last-error=%v", err)
	}
	fmt.Println()
	if reporter, ok := out.(audio.ErrReporter); ok && reporter.Err() != nil {
		return reporter.Err()
	}
	return nil
}

// bars renders band levels as a block ramp, low to high frequency.
func bars(bands []float32) string {
	const ramp = " ▁▂▃▄▅▆▇█"
	runes := []rune(ramp)
	out := make([]rune, len(bands))
	for i, b := range bands {
		if b < 0 {
			b = 0
		} else if b > 1 {
			b = 1
		}
		out[i] = runes[int(float64(b)*float64(len(runes)-1)+0.5)]
	}
	return string(out)
}

// playTone writes a repeating three-beep pattern (0.2s on, 0.2s off, three
// times, then a one-second pause) through the output. The radio stream alone
// is hard to pick out of other players of the same station; the beeps are
// unmistakable.
func playTone(out audio.Output, device string, rate int, secs, volume float64) error {
	if err := out.Open(device); err != nil {
		return err
	}
	const frequency = 880.0 // A5
	beep := func(frame int64) bool {
		cycle := math.Mod(float64(frame)/float64(rate), 2.0)
		return cycle < 0.2 || (cycle >= 0.4 && cycle < 0.6) || (cycle >= 0.8 && cycle < 1.0)
	}
	const chunk = 441 // 10ms at 44.1kHz
	buf := make([]float32, chunk*2)
	var frame int64
	deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
	for time.Now().Before(deadline) {
		for i := 0; i < chunk; i++ {
			var sample float32
			if beep(frame) {
				sample = float32(math.Sin(2*math.Pi*frequency*float64(frame)/float64(rate))) * float32(volume)
			}
			buf[2*i] = sample
			buf[2*i+1] = sample
			frame++
		}
		if err := out.Write(buf); err != nil {
			return fmt.Errorf("write: %w", err)
		}
	}
	fmt.Printf("wrote %d frames (%.1fs), underflow=%v\n", frame, float64(frame)/float64(rate), out.Underflow())
	if reporter, ok := out.(audio.ErrReporter); ok && reporter.Err() != nil {
		return reporter.Err()
	}
	return nil
}

// openStream fetches the stream and starts the MP3 decoder.
func openStream(url string) (io.ReadCloser, beep.StreamSeekCloser, beep.Format, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, beep.Format{}, err
	}
	req.Header.Set("User-Agent", "nghtwv-audiopoc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, beep.Format{}, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, nil, beep.Format{}, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, resp.Status)
	}
	stream, format, err := mp3.Decode(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, nil, beep.Format{}, fmt.Errorf("decode: %w", err)
	}
	return resp.Body, stream, format, nil
}

func gain(v float64) float32 {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	return float32(v)
}
