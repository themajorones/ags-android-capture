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
	adbPath, err := systemADBPath()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("using system ADB executable: %s", adbPath)
	scrcpyPath := envOr("AGS_CAPTURE_SCRCPY", filepath.Join(assetDir, exeName("scrcpy")))
	// AGS can be served from any user-managed machine, hostname, address, or
	// browser port. The helper itself remains bound to loopback.
	allowedOrigins := []string{"*"}

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

func systemADBPath() (string, error) {
	if configured := os.Getenv("AGS_CAPTURE_ADB"); configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("AGS_CAPTURE_ADB does not point to an executable: %w", err)
		}
		return path, nil
	}
	path, err := exec.LookPath("adb")
	if err != nil {
		return "", fmt.Errorf("system ADB was not found on PATH; install Android Platform Tools or set AGS_CAPTURE_ADB: %w", err)
	}
	return path, nil
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
