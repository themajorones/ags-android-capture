package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/themajorones/ags-android-capture/internal/bridge"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: ags-android-capture serve [--listen 127.0.0.1:17653]")
		os.Exit(2)
	}

	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	listenAddress := flags.String("listen", "127.0.0.1:17653", "local browser bridge address")
	useSystemdSocket := flags.Bool("systemd", false, "use the inherited systemd user socket")
	idleTimeout := flags.Duration("idle-timeout", 60*time.Second, "exit after this long without a stream")
	_ = flags.Parse(os.Args[2:])

	executable, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	assetDir := filepath.Dir(executable)
	if runtime.GOOS == "linux" {
		libraryPath := filepath.Join(assetDir, "lib")
		if current := os.Getenv("LD_LIBRARY_PATH"); current != "" {
			libraryPath += string(os.PathListSeparator) + current
		}
		_ = os.Setenv("LD_LIBRARY_PATH", libraryPath)
	}
	adbPath := envOr("AGS_CAPTURE_ADB", systemOrBundledADB(assetDir))
	scrcpyPath := envOr("AGS_CAPTURE_SCRCPY", filepath.Join(assetDir, exeName("scrcpy")))
	allowedOrigins := splitList(os.Getenv("AGS_CAPTURE_ALLOWED_ORIGINS"))
	if len(allowedOrigins) == 0 {
		allowedOrigins = []string{
			"http://localhost:7530", "http://127.0.0.1:7530",
			"http://bigscreen:7530", "http://devnode:7530",
		}
	}

	server := bridge.New(bridge.Config{
		ADBPath:         adbPath,
		ScrcpyPath:      scrcpyPath,
		AllowedOrigins:  allowedOrigins,
		IdleTimeout:     *idleTimeout,
		SessionWaitTime: 20 * time.Second,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.Health)
	mux.HandleFunc("GET /v1/stream", server.Stream)

	listener, err := getListener(*listenAddress, *useSystemdSocket)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go server.ExitWhenIdle(ctx, httpServer)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("AGS capture bridge listening on %s", listener.Addr())
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func systemOrBundledADB(assetDir string) string {
	if path, err := exec.LookPath("adb"); err == nil {
		return path
	}
	return filepath.Join(assetDir, exeName("adb"))
}

func getListener(address string, systemd bool) (net.Listener, error) {
	if systemd {
		if os.Getenv("LISTEN_PID") != fmt.Sprint(os.Getpid()) || os.Getenv("LISTEN_FDS") != "1" {
			return nil, errors.New("systemd socket activation did not provide exactly one listener")
		}
		file := os.NewFile(3, "ags-android-capture.socket")
		if file == nil {
			return nil, errors.New("systemd listener file descriptor is missing")
		}
		listener, err := net.FileListener(file)
		_ = file.Close()
		return listener, err
	}
	if !strings.HasPrefix(address, "127.0.0.1:") && !strings.HasPrefix(address, "[::1]:") {
		return nil, fmt.Errorf("listen address must be loopback only: %s", address)
	}
	return net.Listen("tcp", address)
}

func exeName(name string) string {
	if os.PathSeparator == '\\' {
		return name + ".exe"
	}
	return name
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func splitList(value string) []string {
	var result []string
	for _, entry := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
