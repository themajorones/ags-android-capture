package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
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
	scrcpyID, err := newScrcpySessionID()
	if err != nil {
		return fmt.Errorf("create scrcpy session id: %w", err)
	}
	command := exec.Command(s.config.ScrcpyPath, args...)
	command.Env = withEnvironmentValue(adbEnv, "AGS_SCRCPY_SCID", scrcpyID)
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
	stopStream := func() {
		stopScrcpyServerByID(s.config.ADBPath, adbEnv, streamSerial, scrcpyID)
		terminateProcessTree(command)
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
			stopStream()
			_ = command.Wait()
			return fmt.Errorf("wait for scrcpy stream: %w", result.err)
		}
		if result.width == 0 || result.height == 0 {
			stopStream()
			_ = command.Wait()
			return errors.New("scrcpy returned an empty video size")
		}
		streamWidth, streamHeight = result.width, result.height
	case <-metadataTimer.C:
		stopStream()
		_ = command.Wait()
		return errors.New("timed out waiting for the first Android video frame")
	case <-parent.Done():
		stopStream()
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
		stopStream()
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
	// Stop the Android-side scrcpy process as well as the desktop process. If
	// only the ADB/scrcpy process tree is killed, Android can keep its encoder
	// server alive after Game View closes and interfere with later streams.
	stopStream()
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

func newScrcpySessionID() (string, error) {
	var value [4]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[0] &= 0x7f
	return hex.EncodeToString(value[:]), nil
}

func withEnvironmentValue(environment []string, key, value string) []string {
	entry := key + "=" + value
	result := make([]string, 0, len(environment)+1)
	for _, current := range environment {
		currentKey, _, ok := strings.Cut(current, "=")
		if ok && currentKey == key {
			continue
		}
		result = append(result, current)
	}
	return append(result, entry)
}

func listScrcpyServerProcesses(ctx context.Context, adbPath string, env []string, serial, scrcpyID string) (map[string]string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := runADB(commandCtx, adbPath, env, "-s", serial, "shell", "ps", "-A", "-o", "PID,ARGS")
	if err != nil {
		return nil, fmt.Errorf("list Android scrcpy processes: %s: %w", strings.TrimSpace(output), err)
	}

	processes := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] == "PID" || fields[1] != "app_process" {
			continue
		}
		if fields[2] != "/" || len(fields) < 4 || fields[3] != "com.genymobile.scrcpy.Server" {
			continue
		}
		pid := fields[0]
		if pid == "" || strings.Trim(pid, "0123456789") != "" {
			continue
		}
		args := strings.Join(fields[1:], " ")
		if strings.Contains(args, "scid="+scrcpyID) {
			processes[pid] = args
		}
	}
	return processes, nil
}

func stopScrcpyServerByID(adbPath string, env []string, serial, scrcpyID string) {
	if len(scrcpyID) != 8 || strings.Trim(scrcpyID, "0123456789abcdefABCDEF") != "" {
		log.Printf("invalid scrcpy session id from the video process on %s", serial)
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	current, err := listScrcpyServerProcesses(cleanupCtx, adbPath, env, serial, scrcpyID)
	if err != nil {
		log.Printf("could not find the Android scrcpy process to stop on %s: %v", serial, err)
		return
	}
	var pids []string
	for pid := range current {
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		return
	}
	sort.Strings(pids)
	for _, pid := range pids {
		if _, err := runADB(cleanupCtx, adbPath, env, "-s", serial, "shell", "kill", "-TERM", pid); err != nil {
			log.Printf("could not stop Android scrcpy process %s on %s: %v", pid, serial, err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, err = listScrcpyServerProcesses(cleanupCtx, adbPath, env, serial, scrcpyID)
		if err != nil {
			log.Printf("could not verify Android scrcpy shutdown on %s: %v", serial, err)
			return
		}
		remaining := false
		for _, pid := range pids {
			if _, alive := current[pid]; alive {
				remaining = true
				break
			}
		}
		if !remaining {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pids {
		if _, alive := current[pid]; alive {
			if _, err := runADB(cleanupCtx, adbPath, env, "-s", serial, "shell", "kill", "-KILL", pid); err != nil {
				log.Printf("could not force stop Android scrcpy process %s on %s: %v", pid, serial, err)
			}
		}
	}
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
