package bridge

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

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
