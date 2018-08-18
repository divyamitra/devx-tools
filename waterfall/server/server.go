// Package server implements waterfall service
package server

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"

	empty_pb "github.com/golang/protobuf/ptypes/empty"
	"github.com/waterfall"
	"github.com/waterfall/forward"
	waterfall_grpc "github.com/waterfall/proto/waterfall_go_grpc"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const bufSize = 32 * 1024

// WaterfallServer implements the waterfall gRPC service
type WaterfallServer struct {
	ctx context.Context
}

// Echo exists solely for test purposes. It's a utility function to
// create integration tests between grpc and custom network transports
// like qemu_pipe and usb
func (s *WaterfallServer) Echo(stream waterfall_grpc.Waterfall_EchoServer) error {
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
}

// Push untars a file/dir that it receives over a gRPC stream
func (s *WaterfallServer) Push(rpc waterfall_grpc.Waterfall_PushServer) error {
	xfer, err := rpc.Recv()
	if err != nil {
		return err
	}

	r, w := io.Pipe()

	// Connect the gRPC stream with a reader that untars
	// the contents of the stream into the desired path
	eg, _ := errgroup.WithContext(s.ctx)
	eg.Go(func() error {
		defer r.Close()
		return waterfall.Untar(r, xfer.Path)
	})

	eg.Go(func() error {
		defer w.Close()
		for {
			if err == io.EOF {
				return rpc.SendAndClose(
					&waterfall_grpc.Transfer{Success: true})
			}
			if err != nil {
				return err
			}
			if _, err := w.Write(xfer.Payload); err != nil {
				return rpc.SendAndClose(
					&waterfall_grpc.Transfer{
						Success: false,
						Err:     []byte(err.Error())})
			}
			xfer, err = rpc.Recv()
		}
	})
	return eg.Wait()
}

// Pull tars up a file/dir and sends it over a gRPC stream
func (s *WaterfallServer) Pull(t *waterfall_grpc.Transfer, ps waterfall_grpc.Waterfall_PullServer) error {
	if _, err := os.Stat(t.Path); os.IsNotExist(err) {
		return err
	}

	r, w := io.Pipe()

	// Connect a writer that traverses a the filesystem
	// tars the contents to the gRPC stream
	eg, _ := errgroup.WithContext(s.ctx)
	eg.Go(func() error {
		defer w.Close()
		return waterfall.Tar(w, t.Path)
	})

	buff := make([]byte, bufSize)
	eg.Go(func() error {
		defer r.Close()

		for {
			n, err := r.Read(buff)
			if err != nil && err != io.EOF {
				return err
			}

			if n > 0 {
				// No need to populate anything else. Sending the path
				// everytime is just overhead.
				xfer := &waterfall_grpc.Transfer{Payload: buff[0:n]}
				if err := ps.Send(xfer); err != nil {
					return err
				}
			}

			if err == io.EOF {
				return nil
			}
		}
	})
	err := eg.Wait()
	return err
}

// chanWriter implements Writer and allows us to use the channel for Copy calls
type chanWriter chan []byte

func (c chanWriter) Write(b []byte) (int, error) {
	// make a copy of the slice, the caller might reuse the slice
	// before we have a chance to flush the bytes to the stream.
	nb := make([]byte, len(b))
	copy(nb, b)
	c <- nb
	return len(nb), nil
}

// Exec forks a new process with the desired command and pipes its output to the gRPC stream
func (s *WaterfallServer) Exec(es waterfall_grpc.Waterfall_ExecServer) error {

	// The first message contains the actual command to execute.
	// Implmented as a streaming method in order to support stdin redirection in the future.
	cmdMsg, err := es.Recv()
	if err != nil {
		return err
	}

	if cmdMsg.Cmd.PipeIn {
		// stdin redirection is not implemented. Field exists to avoid future incompatibilities.
		return status.Error(codes.Unimplemented, "stdin redirection is not implemented")
	}

	// Avoid doing any sort of input check, e.g. check that the path exists
	// this way we can forward the exact shell output and exit code.
	cmd := exec.CommandContext(s.ctx, cmdMsg.Cmd.Path, cmdMsg.Cmd.Args...)
	cmd.Dir = cmdMsg.Cmd.Dir
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	// Kill the process in case something went wrong with the connection
	defer cmd.Process.Kill()
	defer cmd.Process.Release()

	// We want to read concurrently from stdout/stderr
	// but we only have one stream to send it to so we need to merge streams before writing.
	// Note that order is not necessarily preserverd.
	stdoutCh := make(chan []byte, 1)
	stderrCh := make(chan []byte, 1)

	eg, _ := errgroup.WithContext(s.ctx)

	// Serialize and multiplex stdout/stderr channels to the grpc stream
	eg.Go(func() error {
		_, err := io.Copy(chanWriter(stdoutCh), stdout)
		close(stdoutCh)
		return err

	})
	eg.Go(func() error {
		_, err := io.Copy(chanWriter(stderrCh), stderr)
		close(stderrCh)
		return err
	})
	eg.Go(func() error {
		var o []byte
		var e []byte
		oo := true
		eo := true
		for oo || eo {
			var msg *waterfall_grpc.CmdProgress
			select {
			case o, oo = <-stdoutCh:
				if !oo {
					break
				}
				msg = &waterfall_grpc.CmdProgress{Stdout: o}
			case e, eo = <-stderrCh:
				if !eo {
					break
				}
				msg = &waterfall_grpc.CmdProgress{Stderr: e}
			}
			if msg != nil {
				if err := es.Send(msg); err != nil {
					return err
				}
			}
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		return err
	}

	if err := cmd.Wait(); err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			if stat, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				return es.Send(&waterfall_grpc.CmdProgress{
					ExitCode: uint32(stat.ExitStatus())})
			}
			// the server always runs on android so the type assertion will aways succeed
			panic("this never happens")
		}
		// we got some other kind of error
		return err
	}
	return es.Send(&waterfall_grpc.CmdProgress{ExitCode: 0})
}

// Forward forwards the grpc stream to the requested port.
func (s *WaterfallServer) Forward(stream waterfall_grpc.Waterfall_ForwardServer) error {
	fwd, err := stream.Recv()
	if err != nil {
		return err
	}

	var kind string
	switch fwd.Kind {
	case waterfall_grpc.ForwardMessage_TCP:
		kind = "tcp"
	case waterfall_grpc.ForwardMessage_UDP:
		kind = "udp"
	case waterfall_grpc.ForwardMessage_UNIX:
		kind = "unix"
	default:
		return status.Error(codes.InvalidArgument, "unsupported network type")
	}

	conn, err := net.Dial(kind, fwd.Addr)
	if err != nil {
		return err
	}
	sf := forward.NewStreamForwarder(stream, conn.(forward.HalfReadWriteCloser))
	return sf.Forward()
}

// Version returns the version of the server.
func (s *WaterfallServer) Version(context.Context, *empty_pb.Empty) (*waterfall_grpc.VersionMessage, error) {
	return &waterfall_grpc.VersionMessage{Version: "0.0"}, nil
}

// NewWaterfallServer returns a gRPC server that implements the waterfall service
func NewWaterfallServer(ctx context.Context) *WaterfallServer {
	return &WaterfallServer{ctx: ctx}
}
