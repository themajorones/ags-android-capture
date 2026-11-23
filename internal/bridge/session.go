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
	"sort"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const maxControlPacket = 256 * 1024

func (s *Server) runStream(parent context.Context, conn *websocket.Conn, request streamRequest) error {
	adbEnv, streamSerial, err := s.connectDevice(parent, request.serial)
	if err != nil {
		return err
	}
	unlockStartup, err := s.acquireDeviceStartup(parent, streamSerial)
	if err != nil {
		return fmt.Errorf("wait to start scrcpy for %s: %w", streamSerial, err)
	}
	startupLocked := true
	defer func() {
		if startupLocked {
			unlockStartup()
		}
	}()
	if err := parent.Err(); err != nil {
		return err
	}

	args := []string{
		"--serial", streamSerial,
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
	unlockStartup()
	startupLocked = false

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

func (s *Server) connectDevice(ctx context.Context, serial string) ([]string, string, error) {
	env := adbEnvironment(s.config.ADBPath, os.Environ())
	connectedSerial := serial
	// AGS shares the user's normal ADB server so its connections are visible
	// to terminal ADB clients. Do not disconnect devices or stop that server
	// when a browser stream closes; both may be in use outside this helper.

	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if output, err := runADB(startCtx, s.config.ADBPath, env, "start-server"); err != nil {
		return nil, "", fmt.Errorf("start system ADB: %s: %w", strings.TrimSpace(output), err)
	}
	candidates := []string{serial}
	if hasHostPort(serial) {
		var err error
		candidates, err = resolveSerialCandidates(ctx, serial)
		if err != nil {
			return nil, "", fmt.Errorf("resolve Android device %s: %w", serial, err)
		}
		var failures []string
		connected := false
		connectDeadline := time.Now().Add(10 * time.Second)
		for _, candidate := range candidates {
			remaining := time.Until(connectDeadline)
			if remaining <= 0 {
				break
			}
			attemptTimeout := min(5*time.Second, remaining)
			connectCtx, connectCancel := context.WithTimeout(ctx, attemptTimeout)
			output, connectErr := runADB(connectCtx, s.config.ADBPath, env, "connect", candidate)
			connectCancel()
			lowerOutput := strings.ToLower(output)
			if connectErr != nil || strings.Contains(lowerOutput, "failed to connect") || strings.Contains(lowerOutput, "cannot connect") {
				failures = append(failures, fmt.Sprintf("%s: %s", candidate, strings.TrimSpace(output)))
				continue
			}

			stateCtx, stateCancel := context.WithTimeout(ctx, 2*time.Second)
			state, stateErr := runADB(stateCtx, s.config.ADBPath, env, "-s", candidate, "get-state")
			stateCancel()
			if stateErr == nil && strings.TrimSpace(state) == "device" {
				connectedSerial = candidate
				connected = true
				break
			}
			failures = append(failures, fmt.Sprintf("%s: %s", candidate, strings.TrimSpace(state)))
		}
		if !connected {
			return nil, "", fmt.Errorf("connect to Android device %s: %s", serial, strings.Join(failures, "; "))
		}
	}

	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		stateCtx, stateCancel := context.WithTimeout(ctx, 2*time.Second)
		output, stateErr := runADB(stateCtx, s.config.ADBPath, env, "-s", connectedSerial, "get-state")
		stateCancel()
		if stateErr == nil && strings.TrimSpace(output) == "device" {
			return env, connectedSerial, nil
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return nil, "", fmt.Errorf("Android device %s did not become ready", connectedSerial)
}

func adbEnvironment(adbPath string, environment []string) []string {
	filtered := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, hasValue := strings.Cut(entry, "=")
		if hasValue && (key == "ADB" || key == "ADB_SERVER_SOCKET" || key == "ANDROID_ADB_SERVER_PORT") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "ADB="+adbPath)
}

// Android endpoint hostnames may resolve to IPv6 link-local addresses before
// usable IPv4 addresses on Windows. ADB's hostname resolver can select that
// unreachable address and fail without trying the other records. Resolve the
// endpoint here, prefer IPv4, and pass the concrete serial to both ADB and
// scrcpy so the chosen route stays consistent for the entire stream.
func resolveSerialCandidates(ctx context.Context, serial string) ([]string, error) {
	host, port, err := net.SplitHostPort(serial)
	if err != nil {
		return nil, err
	}
	addresses := []net.IP{}
	if ip := net.ParseIP(host); ip != nil {
		addresses = append(addresses, ip)
	} else {
		addresses, err = net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("hostname returned no addresses")
	}
	sort.SliceStable(addresses, func(i, j int) bool {
		return addresses[i].To4() != nil && addresses[j].To4() == nil
	})
	candidates := make([]string, 0, len(addresses)+1)
	seen := make(map[string]struct{}, len(addresses)+1)
	for _, address := range addresses {
		candidate := net.JoinHostPort(address.String(), port)
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}
	if net.ParseIP(host) == nil {
		if _, exists := seen[serial]; !exists {
			candidates = append(candidates, serial)
		}
	}
	return candidates, nil
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
