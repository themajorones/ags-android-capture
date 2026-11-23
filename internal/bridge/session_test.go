package bridge

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// The Go test executable doubles as ADB and scrcpy subprocesses in the
// concurrent-stream integration test below.
func init() {
	if os.Getenv("AGS_BRIDGE_TEST_CHILD") != "1" {
		return
	}
	args := strings.Join(os.Args[1:], " ")
	if strings.Contains(args, "--ags-browser-stream") {
		_, _ = os.Stdout.Write([]byte{'A', 'G', 'S', 'C', 0, 0, 3, 32, 0, 0, 2, 88})
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			_, _ = os.Stdout.Write([]byte{0, 0, 0, 1, 0x65, 0x88, 0x84})
		}
	}
	if strings.Contains(args, "get-state") {
		_, _ = io.WriteString(os.Stdout, "device\n")
	}
	os.Exit(0)
}

type shortWriter struct{ data []byte }

func (writer *shortWriter) Write(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	writer.data = append(writer.data, value[0])
	return 1, nil
}

func TestWriteFullHandlesShortWrites(t *testing.T) {
	var writer shortWriter
	if err := writeFull(&writer, []byte("frame")); err != nil {
		t.Fatal(err)
	}
	if got := string(writer.data); got != "frame" {
		t.Fatalf("output = %q", got)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

func TestWriteFullRejectsZeroProgress(t *testing.T) {
	if err := writeFull(zeroWriter{}, []byte("x")); err != io.ErrShortWrite {
		t.Fatalf("error = %v, want %v", err, io.ErrShortWrite)
	}
}

func TestForwardVideoPreservesEncodedBytes(t *testing.T) {
	want := bytes.Repeat([]byte{0, 0, 0, 1, 0x67, 0x64, 0, 0x1f}, 2048)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "done")
		if err := forwardVideo(request.Context(), connection, &chunkReader{data: append([]byte(nil), want...), max: 3}); err != nil {
			t.Errorf("forwardVideo: %v", err)
		}
	}))
	defer server.Close()

	connection, _, err := websocket.Dial(context.Background(), "ws"+server.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "done")
	readContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []byte
	for {
		messageType, chunk, err := connection.Read(readContext)
		if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.MessageBinary {
			t.Fatalf("forwarded message type = %v, want binary", messageType)
		}
		got = append(got, chunk...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("forwarded bytes=%d want=%d", len(got), len(want))
	}
}

func TestForwardInputFramesControlPackets(t *testing.T) {
	want := []byte{1, 2, 3, 4, 5}
	result := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "done")
		var destination bytes.Buffer
		_ = forwardInput(context.Background(), connection, &destination)
		result <- destination.Bytes()
	}))
	defer server.Close()

	connection, _, err := websocket.Dial(context.Background(), "ws"+server.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageBinary, want); err != nil {
		t.Fatal(err)
	}
	_ = connection.Close(websocket.StatusNormalClosure, "done")
	select {
	case got := <-result:
		if !bytes.Equal(got, append([]byte{0, 0, 0, byte(len(want))}, want...)) {
			t.Fatalf("framed input = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forwardInput did not finish after browser disconnect")
	}
}

func TestADBEnvironmentUsesSharedDefaultServer(t *testing.T) {
	environment := adbEnvironment("/usr/bin/adb", []string{
		"PATH=/usr/bin",
		"ADB=/old/adb",
		"ADB_SERVER_SOCKET=tcp:127.0.0.1:5038",
		"ANDROID_ADB_SERVER_PORT=5038",
		"KEEP=value",
	})
	got := strings.Join(environment, "\n")
	if !strings.Contains(got, "ADB=/usr/bin/adb") || !strings.Contains(got, "PATH=/usr/bin") || !strings.Contains(got, "KEEP=value") {
		t.Fatalf("ADB environment lost required values: %q", got)
	}
	if strings.Contains(got, "ADB_SERVER_SOCKET=") || strings.Contains(got, "ANDROID_ADB_SERVER_PORT=") || strings.Contains(got, "ADB=/old/adb") {
		t.Fatalf("ADB environment still overrides the shared server: %q", got)
	}
}

func TestMultipleBrowserStreamsRemainIndependent(t *testing.T) {
	t.Setenv("AGS_BRIDGE_TEST_CHILD", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := New(Config{
		ADBPath:         executable,
		ScrcpyPath:      executable,
		AllowedOrigins:  []string{"*"},
		IdleTimeout:     -1,
		SessionWaitTime: 5 * time.Second,
	})
	httpServer := httptest.NewServer(http.HandlerFunc(server.Stream))
	defer httpServer.Close()
	streamURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/stream?serial="
	dial := func(serial string) *websocket.Conn {
		t.Helper()
		connection, _, dialErr := websocket.Dial(context.Background(), streamURL+serial, &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": []string{"http://ags.example"}},
		})
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		return connection
	}
	read := func(connection *websocket.Conn) (websocket.MessageType, []byte) {
		t.Helper()
		readContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		kind, data, readErr := connection.Read(readContext)
		if readErr != nil {
			t.Fatalf("read concurrent stream: %v", readErr)
		}
		return kind, data
	}
	first := dial("device-1")
	defer first.Close(websocket.StatusNormalClosure, "test complete")
	if kind, data := read(first); kind != websocket.MessageText || !strings.Contains(string(data), `"kind":"metadata"`) {
		t.Fatalf("first stream metadata = (%v, %s)", kind, data)
	}
	if kind, _ := read(first); kind != websocket.MessageBinary {
		t.Fatalf("first stream frame type = %v, want binary", kind)
	}

	second := dial("device-2")
	if kind, data := read(second); kind != websocket.MessageText || !strings.Contains(string(data), `"kind":"metadata"`) {
		t.Fatalf("second stream metadata = (%v, %s)", kind, data)
	}
	if kind, _ := read(second); kind != websocket.MessageBinary {
		t.Fatalf("second stream frame type = %v, want binary", kind)
	}
	waitForStreamCount(t, server, 2)

	// Starting a stream for another device must not cancel or replace this one.
	if kind, _ := read(first); kind != websocket.MessageBinary {
		t.Fatalf("first stream stopped after second connected: message type %v", kind)
	}
	_ = first.Close(websocket.StatusNormalClosure, "close first")
	waitForStreamCount(t, server, 1)
	if kind, _ := read(second); kind != websocket.MessageBinary {
		t.Fatalf("second stream stopped after first closed: message type %v", kind)
	}
	_ = second.Close(websocket.StatusNormalClosure, "close second")
	waitForStreamCount(t, server, 0)
}

func TestNewBrowserStreamReplacesPreviousStreamForSameDevice(t *testing.T) {
	t.Setenv("AGS_BRIDGE_TEST_CHILD", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := New(Config{
		ADBPath:         executable,
		ScrcpyPath:      executable,
		AllowedOrigins:  []string{"*"},
		IdleTimeout:     -1,
		SessionWaitTime: 5 * time.Second,
	})
	httpServer := httptest.NewServer(http.HandlerFunc(server.Stream))
	defer httpServer.Close()
	streamURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/stream?serial=device-1"
	dial := func() *websocket.Conn {
		t.Helper()
		connection, _, dialErr := websocket.Dial(context.Background(), streamURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": []string{"http://ags.example"}},
		})
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		return connection
	}
	read := func(connection *websocket.Conn) (websocket.MessageType, []byte) {
		t.Helper()
		readContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		kind, data, readErr := connection.Read(readContext)
		if readErr != nil {
			t.Fatalf("read stream: %v", readErr)
		}
		return kind, data
	}

	first := dial()
	if kind, data := read(first); kind != websocket.MessageText || !strings.Contains(string(data), `"kind":"metadata"`) {
		t.Fatalf("first stream metadata = (%v, %s)", kind, data)
	}
	if kind, _ := read(first); kind != websocket.MessageBinary {
		t.Fatalf("first stream frame type = %v, want binary", kind)
	}

	second := dial()
	defer second.Close(websocket.StatusNormalClosure, "test complete")
	if kind, data := read(second); kind != websocket.MessageText || !strings.Contains(string(data), `"kind":"metadata"`) {
		t.Fatalf("replacement stream metadata = (%v, %s)", kind, data)
	}
	waitForStreamCount(t, server, 1)
	closeContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, readErr := first.Read(closeContext); readErr == nil {
		t.Fatal("previous stream remained open after replacement")
	}
	if kind, _ := read(second); kind != websocket.MessageBinary {
		t.Fatalf("replacement stream frame type = %v, want binary", kind)
	}
}

func waitForStreamCount(t *testing.T, server *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := server.activeStreamCount(); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active stream count = %d, want %d", server.activeStreamCount(), want)
}

type chunkReader struct {
	data []byte
	max  int
}

func (reader *chunkReader) Read(buffer []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, io.EOF
	}
	count := len(buffer)
	if count > reader.max {
		count = reader.max
	}
	if count > len(reader.data) {
		count = len(reader.data)
	}
	copy(buffer, reader.data[:count])
	reader.data = reader.data[count:]
	return count, nil
}
