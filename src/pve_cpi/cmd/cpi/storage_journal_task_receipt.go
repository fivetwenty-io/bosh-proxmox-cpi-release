package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"io"
	"os"
	"syscall"
)

func readStorageRecoveredTaskEvidence(path string) (evidence *handlers.StorageRecoveredTaskEvidence, retErr error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 || before.Size() <= 0 || before.Size() > 65536 {
		return nil, fmt.Errorf("recovered response evidence must be a bounded private regular file")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		if err := file.Close(); err != nil && retErr == nil {
			evidence = nil
			retErr = fmt.Errorf("close recovered response evidence: %w", err)
		}
	}()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(before, actual) || !actual.Mode().IsRegular() || actual.Mode().Perm()&0o077 != 0 || actual.Size() <= 0 || actual.Size() > 65536 {
		return nil, fmt.Errorf("recovered response evidence changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(data) > 65536 {
		return nil, fmt.Errorf("recovered response evidence exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result handlers.StorageRecoveredTaskEvidence
	if decoder.Decode(&result) != nil {
		return nil, fmt.Errorf("recovered response evidence is malformed")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("recovered response evidence has trailing data")
	}
	return &result, nil
}
