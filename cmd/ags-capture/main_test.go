package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSystemADBPathUsesConfiguredExecutable(t *testing.T) {
	name := "adb"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	adb := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(adb, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGS_CAPTURE_ADB", adb)

	got, err := systemADBPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != adb {
		t.Fatalf("systemADBPath() = %q, want %q", got, adb)
	}
}

func TestSystemADBPathRequiresSystemExecutable(t *testing.T) {
	t.Setenv("AGS_CAPTURE_ADB", "")
	t.Setenv("PATH", t.TempDir())

	if _, err := systemADBPath(); err == nil {
		t.Fatal("systemADBPath() succeeded without a system ADB executable")
	}
}
