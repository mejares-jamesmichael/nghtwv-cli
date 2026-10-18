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

func main() {
	var (
		list   = flag.Bool("list", false, "list output devices and exit")
		device = flag.String("device", "", "stable device name to play through (empty = backend default)")
		secs   = flag.Float64("secs", 10, "seconds to play")
		volume = flag.Float64("volume", 0.6, "software gain, 0..1")
		tone   = flag.Bool("tone", false, "play an 880Hz beep pattern instead of the stream")
		url    = flag.String("url", defaultURL, "stream URL")
		rate   = flag.Int("rate", 44100, "output sample rate")
	)
	flag.Parse()
	if err := run(*list, *device, *secs, *volume, *tone, *url, *rate); err != nil {
		fmt.Fprintln(os.Stderr, "audiopoc:", err)
		os.Exit(1)
	}
}

func run(list bool, device string, secs, volume float64, tone bool, url string, rate int) error {
	out, err := audio.NewOutput(rate)
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
	if list {
		return nil
	}

	chosen := device
	if chosen == "" {
		if defErr != nil {
			return fmt.Errorf("no default device: %w", defErr)
		}
		chosen = def.Name + " (default)"
	}

	if tone {
		fmt.Printf("\nplaying a three-beep pattern for %.0fs through %s at gain %.2f\n", secs, chosen, volume)
		return playTone(out, device, rate, secs, volume)
	}

	fmt.Printf("\nplaying %s for %.0fs through %s at gain %.2f\n", url, secs, chosen, volume)

	body, stream, format, err := openStream(url)
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
	if int(format.SampleRate) != rate {
		source = beep.Resample(4, format.SampleRate, beep.SampleRate(rate), source)
	}

	// Open after the decoder is ready so playback starts with data.
	if err := out.Open(device); err != nil {
		return err
	}

	buf := make([][2]float64, 1024)
	f32 := make([]float32, 0, 2048)
	deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
	written := 0
	for time.Now().Before(deadline) {
		n, ok := source.Stream(buf)
		if n > 0 {
			f32 = f32[:0]
			for _, sample := range buf[:n] {
				f32 = append(f32, gain(sample[0]*volume), gain(sample[1]*volume))
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

	fmt.Printf("wrote %d frames (%.1fs), underflow=%v\n", written, float64(written)/float64(rate), out.Underflow())
	if reporter, ok := out.(audio.ErrReporter); ok && reporter.Err() != nil {
		return reporter.Err()
	}
	return nil
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
