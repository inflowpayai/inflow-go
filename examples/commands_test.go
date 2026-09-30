package examples_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandsRejectMissingCredentials(t *testing.T) {
	directory := os.Getenv("INFLOW_EXAMPLE_COVERAGE_DIR")
	if directory == "" {
		directory = t.TempDir()
	}
	for _, name := range []string{"mpp-buyer", "mpp-seller", "x402-buyer", "x402-seller"} {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), name)
			build := exec.Command("go", "build", "-cover", "-covermode=atomic", "-o", binary, "./"+name)
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, output)
			}
			command := exec.Command(binary)
			command.Env = []string{"GOCOVERDIR=" + directory}
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "set INFLOW_API_KEY") {
				t.Fatalf("exit=%v output=%s", err, output)
			}
		})
	}
}
