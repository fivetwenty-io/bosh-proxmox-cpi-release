package main

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStorageJournalRecoveredTaskFlagsAreCleanupOnlyAndPaired(t *testing.T) {
	base := []string{"cleanup", "--config", "/nonexistent-storage-recovery-test.json", "--allocation-id", "allocation", "--decision-id", "incident", "--authority-id", "writer", "--previous-writer-fenced", "--remote-tasks-settled"}
	step := []string{"--recovered-task-step", "attempt-0-step-1"}
	upid := []string{"--recovered-task-upid", "UPID:pve1:00088AE5:03547171:6AA18319:imgcopy::pmx@pve!pmx:"}
	for _, mode := range []string{"paired", "without receipt", "step only", "task only", "other action", "without settlement"} {
		t.Run(mode, func(t *testing.T) {
			args := append([]string(nil), base...)
			if mode != "task only" {
				args = append(args, step...)
			}
			if mode != "step only" {
				args = append(args, upid...)
			}
			if mode != "without receipt" {
				args = append(args, "--recovered-task-evidence", "/nonexistent-receipt.json")
			}
			if mode == "other action" {
				args[0] = "audit"
			}
			if mode == "without settlement" {
				args = append(args, "--remote-tasks-settled=false")
			}
			var out, err bytes.Buffer
			code := runStorageJournal(args, &out, &err, runOptions{})
			expected := 2
			if mode == "paired" {
				expected = 1
			}
			if code != expected {
				t.Fatalf("code=%d output=%s", code, err.String())
			}
		})
	}
}

func TestStorageRecoveredReceiptReaderRejectsUnsafeInputs(t *testing.T) {
	for _, mode := range []string{"valid", "symlink", "fifo", "public", "oversize", "trailing", "unknown field"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "receipt")
			data := []byte(`{"version":1}`)
			switch mode {
			case "trailing":
				data = []byte(`{"version":1} {}`)
			case "unknown field":
				data = []byte(`{"secret":"never print"}`)
			case "oversize":
				data = bytes.Repeat([]byte("x"), 65537)
			}
			if mode == "fifo" {
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "public" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink" {
				other := filepath.Join(root, "link")
				if err := os.Symlink(path, other); err != nil {
					t.Fatal(err)
				}
				path = other
			}
			_, err := readStorageRecoveredTaskEvidence(path)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe evidence accepted")
			}
		})
	}
}
