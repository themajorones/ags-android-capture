package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const maxControlPacket = 256 * 1024

func (s *Server) runStream(parent context.Context, conn *websocket.Conn, request streamRequest) error {
	adbEnv, cleanupADB, err := s.connectDevice(parent, request.serial)
	if err != nil {
		return err
	}
	defer cleanupADB()

	args := []string{
		"--serial", request.serial,
		"--video-bit-rate", strconv.Itoa(request.bitrate),
		"--max-fps", strconv.Itoa(request.fps),
		"--no-audio", "--no-window", "--no-video-playback",
		"--no-clipboard-autosync", "--ags-browser-stream",
	}
	if request.maxSize > 0 {
		args = append(args, "--max-size", strconv.Itoa(request.maxSize))
	}
	command := exec.Command(s.config.ScrcpyPath, args...)
	command.Env = adbEnv
	configureProcessTree(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open scrcpy video pipe: %w", err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("open scrcpy input pipe: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return fmt.Errorf("open scrcpy log pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start scrcpy: %w", err)
	}
	go func() {
		_, _ = io.Copy(log.Writer(), io.LimitReader(stderr, 1<<20))
	}()

	metadataResult := make(chan struct {
		width  uint32
		height uint32
		err    error
	}, 1)
	go func() {
		preamble := make([]byte, 12)
		_, readErr := io.ReadFull(stdout, preamble)
		if readErr != nil {
			metadataResult <- struct {
				width  uint32
				height uint32
				err    error
			}{err: readErr}
			return
		}
		if string(preamble[:4]) != "AGSC" {
			metadataResult <- struct {
				width  uint32
				height uint32
				err    error
			}{err: fmt.Errorf("scrcpy returned an invalid stream header %q (% x)", string(preamble[:4]), preamble)}
			return
		}
		metadataResult <- struct {
			width  uint32
			height uint32
			err    error
		}{
			width:  binary.BigEndian.Uint32(preamble[4:8]),
			height: binary.BigEndian.Uint32(preamble[8:12]),
		}
	}()

	metadataTimer := time.NewTimer(s.config.SessionWaitTime)
	defer metadataTimer.Stop()
	var streamWidth, streamHeight uint32
	select {
	case result := <-metadataResult:
		if result.err != nil {
			terminateProcessTree(command)
			_ = command.Wait()
			return fmt.Errorf("wait for scrcpy stream: %w", result.err)
		}
		if result.width == 0 || result.height == 0 {
			terminateProcessTree(command)
			_ = command.Wait()
			return errors.New("scrcpy returned an empty video size")
		}
		streamWidth, streamHeight = result.width, result.height
	case <-metadataTimer.C:
		terminateProcessTree(command)
		_ = command.Wait()
		return errors.New("timed out waiting for the first Android video frame")
	case <-parent.Done():
		terminateProcessTree(command)
		_ = command.Wait()
		return parent.Err()
	}

	metadata, _ := json.Marshal(map[string]any{
		"kind": "metadata", "width": streamWidth, "height": streamHeight,
		"codec": "h264", "transport": "scrcpy",
	})
	if err := writeWebSocket(parent, conn, websocket.MessageText, metadata); err != nil {
		terminateProcessTree(command)
		_ = command.Wait()
		return err
	}

	outputDone := make(chan error, 1)
	inputDone := make(chan error, 1)
	go func() { outputDone <- forwardVideo(parent, conn, stdout) }()
	go func() { inputDone <- forwardInput(parent, conn, stdin) }()

	var streamErr error
	outputRead := false
	select {
	case streamErr = <-outputDone:
		outputRead = true
	case streamErr = <-inputDone:
	case <-parent.Done():
		streamErr = parent.Err()
	}
	_ = stdin.Close()
	// scrcpy starts adb shell as a child. Kill the whole process tree so an
	// abandoned adb child cannot keep stdout open and strand the socket service.
	terminateProcessTree(command)
	if !outputRead {
		select {
		case <-outputDone:
		case <-time.After(3 * time.Second):
			streamErr = errors.New("scrcpy process tree did not stop after stream closure")
		}
	}

	waitErr := command.Wait()
	closeStatus := websocket.CloseStatus(streamErr)
	if streamErr != nil {
		if !errors.Is(streamErr, context.Canceled) && closeStatus != websocket.StatusNormalClosure && closeStatus != websocket.StatusGoingAway {
			return streamErr
		}
	}
	if waitErr != nil && parent.Err() == nil {
		return fmt.Errorf("scrcpy exited: %w", waitErr)
	}
	return nil
}

func (s *Server) connectDevice(ctx context.Context, serial string) ([]string, func(), error) {
	port, err := reserveLoopbackPort()
	if err != nil {
		return nil, func() {}, err
	}
	adbPort := strconv.Itoa(port)
	env := append(os.Environ(),
		"ADB="+s.config.ADBPath,
		"ANDROID_ADB_SERVER_PORT="+adbPort,
	)
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if hasHostPort(serial) {
			_, _ = runADB(cleanupCtx, s.config.ADBPath, env, "disconnect", serial)
		}
		_, _ = runADB(cleanupCtx, s.config.ADBPath, env, "kill-server")
	}

	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if output, err := runADB(startCtx, s.config.ADBPath, env, "start-server"); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("start packaged ADB: %s: %w", strings.TrimSpace(output), err)
	}
	if hasHostPort(serial) {
		connectCtx, connectCancel := context.WithTimeout(ctx, 10*time.Second)
		output, err := runADB(connectCtx, s.config.ADBPath, env, "connect", serial)
		connectCancel()
		if err != nil || strings.Contains(strings.ToLower(output), "failed to connect") {
			cleanup()
			return nil, func() {}, fmt.Errorf("connect to Android device %s: %s", serial, strings.TrimSpace(output))
		}
	}

	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		stateCtx, stateCancel := context.WithTimeout(ctx, 2*time.Second)
		output, stateErr := runADB(stateCtx, s.config.ADBPath, env, "-s", serial, "get-state")
		stateCancel()
		if stateErr == nil && strings.TrimSpace(output) == "device" {
			return env, cleanup, nil
		}
		select {
		case <-ctx.Done():
			cleanup()
			return nil, func() {}, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	cleanup()
	return nil, func() {}, fmt.Errorf("Android device %s did not become ready", serial)
}

func runADB(ctx context.Context, executable string, env []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = env
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	return output.String(), err
}

func reserveLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func hasHostPort(serial string) bool {
	_, _, err := net.SplitHostPort(serial)
	if err == nil {
		return true
	}
	// ADB serials conventionally use host:port without IPv6 brackets.
	return strings.Count(serial, ":") == 1 && strings.LastIndex(serial, ":") > 0
}

func forwardVideo(ctx context.Context, conn *websocket.Conn, source io.Reader) error {
	buffer := make([]byte, 64*1024)
	for {
		count, err := source.Read(buffer)
		if count > 0 {
			if writeErr := writeWebSocket(ctx, conn, websocket.MessageBinary, buffer[:count]); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func forwardInput(ctx context.Context, conn *websocket.Conn, destination io.Writer) error {
	for {
		messageType, packet, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageBinary || len(packet) == 0 || len(packet) > maxControlPacket {
			return errors.New("browser sent an invalid input packet")
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(packet)))
		if err := writeFull(destination, size[:]); err != nil {
			return err
		}
		if err := writeFull(destination, packet); err != nil {
			return err
		}
	}
}

func writeWebSocket(ctx context.Context, conn *websocket.Conn, kind websocket.MessageType, data []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, kind, data)
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func executableInPath(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func normalizeExecutable(path string) string {
	if filepath.IsAbs(path) || strings.ContainsRune(path, os.PathSeparator) {
		return path
	}
	return filepath.Clean(path)
}
