package service

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mejares-jamesmichael/nghtwv-cli/internal/player"
)

// Server limits mirror the mpv IPC client's discipline: requests are tiny
// (no audio crosses this wire), so a 64 KiB line cap is generous, and one
// request may not hold the backend forever.
const (
	requestLimit   = 64 * 1024
	requestTimeout = 20 * time.Second
)

// Server accepts clients on one endpoint and dispatches verbs to a
// backend. A stop request is acknowledged, then shuts the server down.
type Server struct {
	backend Backend
	ln      net.Listener
	proto   int

	stopOnce sync.Once
	stopped  chan struct{}
}

// NewServer binds a server to an already-listening endpoint. Tests pass a
// loopback listener; the hidden service verb passes the platform endpoint.
func NewServer(backend Backend, ln net.Listener) *Server {
	return &Server{
		backend: backend,
		ln:      ln,
		proto:   Protocol,
		stopped: make(chan struct{}),
	}
}

// SetProtocol overrides the wire version this server speaks; tests use it
// to act as a service from another revision.
func (s *Server) SetProtocol(proto int) { s.proto = proto }

// Serve accepts connections until a stop request or a cancelled context
// shuts the server down. A clean shutdown returns nil; an accept failure
// that was not a shutdown passes the error through. In-flight requests —
// including the acknowledgement of the stop that triggered the shutdown —
// finish before Serve returns.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		select {
		case <-ctx.Done():
			s.shutdown()
		case <-s.stopped:
		}
	}()

	var wg sync.WaitGroup
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			wg.Wait()
			select {
			case <-s.stopped:
				return nil
			default:
				return err
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(nc)
		}()
	}
}

// shutdown closes the endpoint once; it is idempotent so a stop request, a
// signal, and a failed accept cannot race each other.
func (s *Server) shutdown() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		_ = s.ln.Close()
	})
}

// handle serves one connection: newline-delimited requests, one reply per
// request, several requests per connection allowed.
func (s *Server) handle(nc net.Conn) {
	defer nc.Close()
	scanner := bufio.NewScanner(nc)
	scanner.Buffer(make([]byte, 0, 4096), requestLimit)
	for scanner.Scan() {
		reply, stop := s.dispatch(scanner.Bytes())
		if _, err := nc.Write(append(reply, '\n')); err != nil {
			return
		}
		if stop {
			s.shutdown()
			return
		}
	}
	if scanner.Err() != nil {
		b, _ := json.Marshal(Reply{Proto: s.proto, Code: CodeBadRequest, Error: "request too large"})
		_, _ = nc.Write(append(b, '\n'))
	}
}

// dispatch decodes one request line and returns the reply bytes. The
// second result marks a frozen stop: the caller acknowledges it, then
// shuts the server down.
func (s *Server) dispatch(line []byte) ([]byte, bool) {
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		return mustJSON(Reply{Proto: s.proto, Code: CodeBadRequest, Error: "bad request: " + err.Error()}), false
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	// Rule 3: the frozen verb runs before version gating, with no fields
	// required beyond cmd and id.
	if req.Cmd == CmdStop {
		err := s.backend.Stop(ctx)
		reply := Reply{ID: req.ID, OK: err == nil, Proto: s.proto}
		if err != nil {
			reply.Error = err.Error()
			reply.Code = CodeBackend
		}
		return mustJSON(reply), true
	}

	if req.Proto != s.proto {
		return mustJSON(Reply{
			ID:    req.ID,
			Proto: s.proto,
			Code:  CodeProtocolMismatch,
			Error: fmt.Sprintf("service speaks protocol %d, client speaks %d", s.proto, req.Proto),
		}), false
	}

	switch req.Cmd {
	case CmdPing:
		// Cheap liveness probe: no backend work, any reply proves a service
		// is there (the version gate above still applies, and its refusal
		// is a reply too).
		return mustJSON(Reply{ID: req.ID, OK: true, Proto: s.proto}), false
	case CmdStatus:
		return mustJSON(Reply{
			ID: req.ID, OK: true, Proto: s.proto,
			Result: mustJSON(s.backend.Status(ctx)),
		}), false
	case CmdPlay:
		st, err := s.backend.Play(ctx)
		return s.stateReply(req.ID, st, err), false
	case CmdPause:
		st, err := s.backend.Pause(ctx)
		return s.stateReply(req.ID, st, err), false
	case CmdToggle:
		st, err := s.backend.Toggle(ctx)
		return s.stateReply(req.ID, st, err), false
	case CmdVolume:
		var args VolumeArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return mustJSON(Reply{
				ID: req.ID, Proto: s.proto, Code: CodeBadRequest,
				Error: "volume: bad args: " + err.Error(),
			}), false
		}
		st, err := s.backend.Volume(ctx, args.Value)
		return s.stateReply(req.ID, st, err), false
	case CmdOutputs:
		devices, err := s.backend.Outputs(ctx)
		if err != nil {
			return s.errorReply(req.ID, err), false
		}
		return mustJSON(Reply{
			ID: req.ID, OK: true, Proto: s.proto,
			Result: mustJSON(OutputsResult{Outputs: devices}),
		}), false
	case CmdOutput:
		var args OutputArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return mustJSON(Reply{
				ID: req.ID, Proto: s.proto, Code: CodeBadRequest,
				Error: "output: bad args: " + err.Error(),
			}), false
		}
		st, err := s.backend.SetOutput(ctx, args.Device)
		return s.stateReply(req.ID, st, err), false
	case CmdSpectrum:
		// Optional backend capability: without a sample tap the reply is a
		// definite "unsupported" so clients stop asking.
		provider, ok := s.backend.(SpectrumProvider)
		if !ok {
			return mustJSON(Reply{
				ID: req.ID, Proto: s.proto, Code: CodeUnsupported,
				Error: "spectrum is not supported by this backend",
			}), false
		}
		return mustJSON(Reply{
			ID: req.ID, OK: true, Proto: s.proto,
			Result: mustJSON(provider.Spectrum()),
		}), false
	default:
		return mustJSON(Reply{
			ID: req.ID, Proto: s.proto, Code: CodeUnknownCommand,
			Error: fmt.Sprintf("unknown command %q", req.Cmd),
		}), false
	}
}

// stateReply wraps a transport result: state on success, backend error text
// on failure.
func (s *Server) stateReply(id int64, st player.State, err error) []byte {
	if err != nil {
		return s.errorReply(id, err)
	}
	return mustJSON(Reply{ID: id, OK: true, Proto: s.proto, Result: mustJSON(st)})
}

// errorReply reports a backend verb failure.
func (s *Server) errorReply(id int64, err error) []byte {
	return mustJSON(Reply{ID: id, Proto: s.proto, Code: CodeBackend, Error: err.Error()})
}

// mustJSON encodes a value for the wire; our shapes cannot fail to encode,
// so a failure collapses to a bad_request reply instead of panicking.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(Reply{Code: CodeBadRequest, Error: "encode: " + err.Error()})
	}
	return b
}
