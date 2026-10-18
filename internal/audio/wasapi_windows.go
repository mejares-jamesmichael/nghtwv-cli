//go:build windows

package audio

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/degubites/go-wca/pkg/wca"
	"github.com/go-ole/go-ole"
)

const (
	// waveFormatIEEEFloat is WAVE_FORMAT_IEEE_FLOAT. The fork of go-wca this
	// project depends on does not carry WAVEFORMATEXTENSIBLE yet, but plain
	// WAVEFORMATEX is sufficient for float32 stereo.
	waveFormatIEEEFloat = 0x0003

	// wasapiBufferDuration is the shared-mode engine buffer in 100ns units.
	wasapiBufferDuration = 2000000 // 200ms

	// wasapiPollInterval paces the render loop; the padding check keeps the
	// engine buffer topped up.
	wasapiPollInterval = 5 * time.Millisecond
)

// wasapiOutput implements Output on top of WASAPI (shared mode, timer driven)
// through pure-Go COM bindings.
//
// COM discipline: every entry point that touches a COM interface runs inside
// withCOM, which locks its goroutine to an OS thread and initializes the
// multithreaded apartment there. COM handles are created during Open and
// released by the render goroutine that used them.
type wasapiOutput struct {
	rate int

	mu      sync.Mutex
	ring    *pcmRing
	closed  bool
	primed  bool
	starved bool
	err     error

	// COM handles; owned by the render goroutine after Open returns.
	device       *wca.IMMDevice
	client       *wca.IAudioClient
	render       *wca.IAudioRenderClient
	bufferFrames uint32

	stop chan struct{}
	done chan struct{}
}

// NewOutput returns a WASAPI output that renders at the given sample rate.
func NewOutput(rate int) (Output, error) {
	return &wasapiOutput{rate: rate}, nil
}

func (o *wasapiOutput) Devices() ([]Device, error) {
	var devices []Device
	err := withCOM(func() error {
		enumerator, err := newDeviceEnumerator()
		if err != nil {
			return err
		}
		defer enumerator.Release()
		return forEachActiveRenderDevice(enumerator, func(dev *wca.IMMDevice) error {
			id, err := deviceID(dev)
			if err != nil {
				return err
			}
			description, _ := deviceFriendlyName(dev)
			devices = append(devices, Device{Name: id, Description: description})
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("audio: list devices: %w", err)
	}
	return devices, nil
}

func (o *wasapiOutput) Default() (Device, error) {
	var device Device
	err := withCOM(func() error {
		enumerator, err := newDeviceEnumerator()
		if err != nil {
			return err
		}
		defer enumerator.Release()
		var dev *wca.IMMDevice
		if err := enumerator.GetDefaultAudioEndpoint(uint32(wca.ERender), wca.DEVICE_STATE_ACTIVE, &dev); err != nil {
			return err
		}
		defer dev.Release()
		id, err := deviceID(dev)
		if err != nil {
			return err
		}
		description, _ := deviceFriendlyName(dev)
		device = Device{Name: id, Description: description}
		return nil
	})
	if err != nil {
		return Device{}, fmt.Errorf("audio: default device: %w", err)
	}
	return device, nil
}

func (o *wasapiOutput) Open(device string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return ErrClosed
	}
	if o.client != nil {
		return errors.New("audio: already open")
	}
	if err := withCOM(func() error { return o.open(device) }); err != nil {
		_ = withCOM(func() error { o.release(); return nil })
		return fmt.Errorf("audio: open %q: %w", device, err)
	}
	// Half a second of stereo frames of decode-ahead.
	o.ring = newPCMRing(o.rate)
	o.stop = make(chan struct{})
	o.done = make(chan struct{})
	go o.renderLoop()
	return nil
}

// open creates and starts the stream. It runs inside withCOM.
func (o *wasapiOutput) open(name string) error {
	enumerator, err := newDeviceEnumerator()
	if err != nil {
		return err
	}
	defer enumerator.Release()

	var dev *wca.IMMDevice
	if name == "" {
		err = enumerator.GetDefaultAudioEndpoint(uint32(wca.ERender), wca.DEVICE_STATE_ACTIVE, &dev)
	} else {
		err = enumerator.GetDevice(name, &dev)
	}
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNoDevice, name)
	}
	o.device = dev

	var client *wca.IAudioClient
	if err := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &client); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	o.client = client

	format := wca.WAVEFORMATEX{
		WFormatTag:      waveFormatIEEEFloat,
		NChannels:       2,
		NSamplesPerSec:  uint32(o.rate),
		NAvgBytesPerSec: uint32(o.rate * 2 * 4),
		NBlockAlign:     2 * 4,
		WBitsPerSample:  32,
	}
	// Let the audio engine convert our float32 stream to the endpoint's mix
	// format instead of shipping a resampler in the hot path.
	flags := uint32(wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY)
	if err := client.Initialize(uint32(wca.AUDCLNT_SHAREMODE_SHARED), flags, wasapiBufferDuration, 0, &format, nil); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	var render *wca.IAudioRenderClient
	if err := client.GetService(wca.IID_IAudioRenderClient, &render); err != nil {
		return fmt.Errorf("render service: %w", err)
	}
	o.render = render

	if err := client.GetBufferSize(&o.bufferFrames); err != nil {
		return fmt.Errorf("buffer size: %w", err)
	}
	if err := client.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return nil
}

// renderLoop owns the COM handles after Open: it tops up the engine buffer
// from the ring, and releases the handles while the apartment is still up.
func (o *wasapiOutput) renderLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(o.done)
	if err := comInit(); err != nil {
		// The apartment never came up; release under a fresh initialization
		// so the handles are not leaked.
		_ = withCOM(func() error { o.release(); return nil })
		o.fail(err)
		return
	}
	// Defers run in reverse, so release() runs first: COM interfaces must be
	// released before CoUninitialize, and o.done closes only after both.
	defer ole.CoUninitialize()
	defer o.release()

	for {
		select {
		case <-o.stop:
			o.client.Stop()
			return
		default:
		}
		var padding uint32
		if err := o.client.GetCurrentPadding(&padding); err != nil {
			o.fail(fmt.Errorf("get padding: %w", err))
			return
		}
		if free := o.bufferFrames - padding; free > 0 {
			var data *byte
			if err := o.render.GetBuffer(free, &data); err != nil {
				o.fail(fmt.Errorf("get buffer: %w", err))
				return
			}
			dst := unsafe.Slice((*float32)(unsafe.Pointer(data)), int(free)*2)
			n := o.ring.Read(dst)
			if n < len(dst) {
				clear(dst[n:])
				// Silence before the first sample is the caller warming up,
				// not starvation.
				o.mu.Lock()
				primed := o.primed
				o.mu.Unlock()
				if primed {
					o.markStarved()
				}
			}
			if n > 0 {
				o.mu.Lock()
				o.primed = true
				o.mu.Unlock()
			}
			if err := o.render.ReleaseBuffer(free, 0); err != nil {
				o.fail(fmt.Errorf("release buffer: %w", err))
				return
			}
		}
		time.Sleep(wasapiPollInterval)
	}
}

// fail records an asynchronous failure and closes the ring so blocked and
// future writers observe it.
func (o *wasapiOutput) fail(err error) {
	o.mu.Lock()
	if o.err == nil {
		o.err = err
	}
	ring := o.ring
	o.mu.Unlock()
	if ring != nil {
		ring.Close()
	}
}

// release drops the COM handles. It must run on a COM-initialized thread.
func (o *wasapiOutput) release() {
	if o.render != nil {
		o.render.Release()
		o.render = nil
	}
	if o.client != nil {
		o.client.Release()
		o.client = nil
	}
	if o.device != nil {
		o.device.Release()
		o.device = nil
	}
}

func (o *wasapiOutput) Write(frames []float32) error {
	o.mu.Lock()
	ring, closed := o.ring, o.closed
	o.mu.Unlock()
	if ring == nil || closed {
		return ErrClosed
	}
	if _, err := ring.Write(frames); err != nil {
		if errors.Is(err, errRingClosed) {
			return ErrClosed
		}
		return err
	}
	return nil
}

// Flush drops everything the ring has buffered. Audio already handed to the
// WASAPI engine buffer (up to wasapiBufferDuration) still plays out.
func (o *wasapiOutput) Flush() error {
	o.mu.Lock()
	ring, closed := o.ring, o.closed
	o.mu.Unlock()
	if ring == nil || closed {
		return ErrClosed
	}
	ring.Reset()
	return nil
}

func (o *wasapiOutput) markStarved() {
	o.mu.Lock()
	o.starved = true
	o.mu.Unlock()
}

func (o *wasapiOutput) Underflow() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.starved
}

// Err returns the asynchronous failure, if any.
func (o *wasapiOutput) Err() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *wasapiOutput) Close() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	ring, stop, done := o.ring, o.stop, o.done
	o.mu.Unlock()
	if ring != nil {
		ring.Close()
	}
	if stop != nil {
		close(stop)
		<-done
	}
	return nil
}

// comInit initializes the calling thread's COM apartment. withCOM pairs it
// with ole.CoUninitialize.
func comInit() error {
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		var oleErr *ole.OleError
		if errors.As(err, &oleErr) && oleErr.Code() == 1 { // S_FALSE: already initialized
			return nil
		}
		return fmt.Errorf("com init: %w", err)
	}
	return nil
}

// withCOM runs f on a locked OS thread with COM initialized (multithreaded
// apartment, which is what WASAPI clients should use).
func withCOM(f func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := comInit(); err != nil {
		return err
	}
	defer ole.CoUninitialize()
	return f()
}

func newDeviceEnumerator() (*wca.IMMDeviceEnumerator, error) {
	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &enumerator); err != nil {
		return nil, fmt.Errorf("device enumerator: %w", err)
	}
	return enumerator, nil
}

func forEachActiveRenderDevice(enumerator *wca.IMMDeviceEnumerator, fn func(*wca.IMMDevice) error) error {
	var collection *wca.IMMDeviceCollection
	if err := enumerator.EnumAudioEndpoints(uint32(wca.ERender), wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return fmt.Errorf("enum endpoints: %w", err)
	}
	defer collection.Release()
	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return fmt.Errorf("endpoint count: %w", err)
	}
	for i := uint32(0); i < count; i++ {
		var dev *wca.IMMDevice
		if err := collection.Item(i, &dev); err != nil {
			return fmt.Errorf("endpoint %d: %w", i, err)
		}
		err := fn(dev)
		dev.Release()
		if err != nil {
			return err
		}
	}
	return nil
}

func deviceID(dev *wca.IMMDevice) (string, error) {
	var id string
	if err := dev.GetId(&id); err != nil {
		return "", fmt.Errorf("device id: %w", err)
	}
	return id, nil
}

func deviceFriendlyName(dev *wca.IMMDevice) (string, error) {
	var store *wca.IPropertyStore
	if err := dev.OpenPropertyStore(wca.STGM_READ, &store); err != nil {
		return "", fmt.Errorf("property store: %w", err)
	}
	defer store.Release()
	value := wca.PROPVARIANT{}
	if err := store.GetValue(&wca.PKEY_Device_FriendlyName, &value); err != nil {
		return "", fmt.Errorf("friendly name: %w", err)
	}
	return value.String(), nil
}
