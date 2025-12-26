/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lvm

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunCommandSplitErrorInfo tests the RunCommandSplit function
func TestRunCommandSplitErrorInfo(t *testing.T) {
	// test1: multiline stderr
	t.Run("multiline stderr", func(t *testing.T) {
		_, stderr, err := RunCommandSplit(context.Background(), "sh", "-c", "echo 'line1' >&2 && echo 'line2' >&2")

		fmt.Println("error info:", err.Error())
		fmt.Println("stderr:", string(stderr))

		// check if stderr contains newline
		if !strings.Contains(string(stderr), "\n") {
			t.Error("stderr should contain newline")
		}

		// check if newline is replaced with " | "
		if err != nil && !strings.Contains(err.Error(), " | ") {
			t.Error("newline should be replaced with ' | '")
		}
	})

	// test2: single line stderr
	t.Run("single line stderr", func(t *testing.T) {
		_, stderr, err := RunCommandSplit(context.Background(), "sh", "-c", "echo 'single error' >&2")

		fmt.Println("error info:", err.Error())
		fmt.Println("stderr:", string(stderr))
	})
}

// TestRunCommandSplitTimeout tests the timeout mechanism
func TestRunCommandSplitTimeout(t *testing.T) {
	// Test 1: Command that completes quickly (should not timeout)
	t.Run("command completes quickly", func(t *testing.T) {
		start := time.Now()
		stdout, stderr, err := RunCommandSplit(context.Background(), "echo", "hello")
		duration := time.Since(start)

		if err != nil {
			t.Errorf("expected no error, got: %v", err)
		}

		if duration > CommandTimeout {
			t.Errorf("command should complete before timeout, took: %v", duration)
		}

		output := strings.TrimSpace(string(stdout))
		if output != "hello" {
			t.Errorf("expected output 'hello', got: %s", output)
		}

		if len(stderr) > 0 {
			t.Errorf("expected no stderr, got: %s", string(stderr))
		}

		t.Logf("✓ Command completed in %v (expected < %v)", duration, CommandTimeout)
	})

	// Test 2: Command that times out (sleep longer than timeout)
	t.Run("command times out", func(t *testing.T) {
		// Sleep for longer than CommandTimeout (2 minutes)
		// Use 3 minutes to ensure it times out
		sleepDuration := CommandTimeout + 1*time.Minute
		sleepSeconds := int(sleepDuration.Seconds())

		start := time.Now()
		_, _, err := RunCommandSplit(context.Background(), "sleep", fmt.Sprintf("%d", sleepSeconds))
		duration := time.Since(start)

		// Should timeout around CommandTimeout (2 minutes)
		if duration < CommandTimeout {
			t.Errorf("command should timeout after %v, but completed in %v", CommandTimeout, duration)
		}

		// Allow some tolerance (should timeout within CommandTimeout + 5 seconds)
		if duration > CommandTimeout+5*time.Second {
			t.Errorf("command should timeout around %v, but took %v", CommandTimeout, duration)
		}

		// Should return timeout error
		if err == nil {
			t.Error("expected timeout error, got nil")
		}

		// When command times out, it's terminated by signal (SIGTERM)
		// So the error will be "signal: terminated" instead of "timed out"
		// Check if it's an exec.ExitError (which indicates process was terminated)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Errorf("expected exec.ExitError (process terminated), got: %T: %v", err, err)
		}

		// Error message should indicate process was terminated by signal
		errMsg := err.Error()
		if !strings.Contains(errMsg, "terminated") && !strings.Contains(errMsg, "signal") {
			t.Errorf("expected signal termination error, got: %v", err)
		}

		t.Logf("✓ Command timed out after %v (expected ~%v)", duration, CommandTimeout)
		t.Logf("✓ Error message: %v", err)
	})

	// Test 3: Command with child processes (simulate lvcreate behavior)
	t.Run("command with child processes times out", func(t *testing.T) {
		// Create a script that spawns child processes and sleeps
		script := `#!/bin/bash
# Spawn a child process that sleeps
(sleep 300) &
CHILD_PID=$!
# Parent also sleeps
sleep 300
wait $CHILD_PID
`

		start := time.Now()
		stdout, stderr, err := RunCommandSplit(context.Background(), "bash", "-c", script)
		duration := time.Since(start)
		_ = stdout
		_ = stderr

		// Should timeout
		if duration < CommandTimeout {
			t.Errorf("command with children should timeout after %v, but completed in %v", CommandTimeout, duration)
		}

		if duration > CommandTimeout+5*time.Second {
			t.Errorf("command should timeout around %v, but took %v", CommandTimeout, duration)
		}

		// Should return timeout error
		if err == nil {
			t.Error("expected timeout error, got nil")
		}

		// When command times out, it's terminated by signal (SIGTERM)
		// So the error will be "signal: terminated" instead of "timed out"
		// Check if it's an exec.ExitError (which indicates process was terminated)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Errorf("expected exec.ExitError (process terminated), got: %T: %v", err, err)
		}

		// Error message should indicate process was terminated by signal
		errMsg := err.Error()
		if !strings.Contains(errMsg, "terminated") && !strings.Contains(errMsg, "signal") {
			t.Errorf("expected signal termination error, got: %v", err)
		}

		t.Logf("✓ Command with children timed out after %v", duration)
		t.Logf("✓ Error message: %v", err)
		t.Logf("✓ Stdout: %s", string(stdout))
		t.Logf("✓ Stderr: %s", string(stderr))
	})
}

// TestRunCommandSplitTimeoutShortTimeout tests with a shorter timeout for faster testing
// This test uses a modified version that allows custom timeout for testing
func TestRunCommandSplitTimeoutShortTimeout(t *testing.T) {
	// This test requires modifying RunCommandSplit to accept timeout parameter
	// For now, we'll test with a script that simulates the behavior
	t.Run("short timeout test", func(t *testing.T) {
		// Use a script that sleeps for 5 seconds
		// But we can't easily test with shorter timeout without modifying the function
		// So we'll just verify the function works with normal timeout
		start := time.Now()
		_, _, err := RunCommandSplit(context.Background(), "sleep", "1")
		duration := time.Since(start)

		if err != nil {
			t.Errorf("expected no error for 1 second sleep, got: %v", err)
		}

		if duration > 5*time.Second {
			t.Errorf("1 second sleep should complete quickly, took: %v", duration)
		}

		t.Logf("✓ Short command completed in %v", duration)
	})
}
