package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevelopmentLauncherPreservesCallerDirectoryAndKeepsStopAvailable(t *testing.T) {
	fixture := t.TempDir()
	repoDir := filepath.Join(fixture, "repo")
	scriptsDir := filepath.Join(repoDir, "scripts")
	callerDir := filepath.Join(fixture, "caller")
	binDir := filepath.Join(fixture, "bin")
	cacheDir := filepath.Join(fixture, "cache")
	for _, dir := range []string{scriptsDir, callerDir, binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	callerDir, err := filepath.EvalSymlinks(callerDir)
	if err != nil {
		t.Fatalf("resolve caller directory: %v", err)
	}

	launcher, err := os.ReadFile(filepath.Join("..", "..", "scripts", "te-dev"))
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	launcherPath := filepath.Join(scriptsDir, "te-dev")
	if err := os.WriteFile(launcherPath, launcher, 0o755); err != nil {
		t.Fatalf("write launcher: %v", err)
	}
	if err := os.Symlink(launcherPath, filepath.Join(binDir, "te")); err != nil {
		t.Fatalf("symlink launcher: %v", err)
	}

	writeLauncherFixtureFile(t, filepath.Join(repoDir, "Makefile"), "build-go-only:\n\t@true\n", 0o644)
	writeLauncherFixtureFile(t, filepath.Join(repoDir, "te"), "#!/bin/sh\nprintf '%s\\n' \"$PWD\"\nprintf '%s\\n' \"$*\"\n", 0o755)

	output := runDevelopmentLauncher(t, filepath.Join(binDir, "te"), callerDir, cacheDir, "study", "execution", "--csv", "results.csv")
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 2 || lines[0] != callerDir || lines[1] != "study execution --csv results.csv" {
		t.Fatalf("launcher output = %q, want caller directory and forwarded arguments", output)
	}
	badCache := filepath.Join(fixture, "cache-is-a-file")
	writeLauncherFixtureFile(t, badCache, "not a directory", 0o644)
	output = runDevelopmentLauncher(t, filepath.Join(binDir, "te"), callerDir, badCache, "status")
	lines = strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 || lines[len(lines)-2] != callerDir || lines[len(lines)-1] != "status" {
		t.Fatalf("unwritable-cache output = %q, want warning followed by executed command", output)
	}

	if err := os.Remove(filepath.Join(repoDir, "te")); err != nil {
		t.Fatalf("remove checkout binary: %v", err)
	}
	marker := filepath.Join(fixture, "unexpected-build")
	failingMakefile := "build-go-only:\n\t@touch " + marker + "\n\t@false\n"
	writeLauncherFixtureFile(t, filepath.Join(repoDir, "Makefile"), failingMakefile, 0o644)

	output = runDevelopmentLauncher(t, filepath.Join(binDir, "te"), callerDir, cacheDir, "stop")
	lines = strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 2 || lines[0] != callerDir || lines[1] != "stop" {
		t.Fatalf("cached stop output = %q, want caller directory and stop argument", output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stop attempted a rebuild; marker stat error = %v", err)
	}
}

func writeLauncherFixtureFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runDevelopmentLauncher(t *testing.T, launcher, dir, cacheDir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(launcher, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+cacheDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run launcher %v: %v\n%s", args, err, output)
	}
	return string(output)
}
