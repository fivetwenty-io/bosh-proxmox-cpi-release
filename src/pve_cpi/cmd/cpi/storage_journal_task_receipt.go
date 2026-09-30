package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"io"
	"io/fs"
	"os"
	"syscall"
)

func readStorageRecoveredTaskEvidence(path string) (evidence *handlers.StorageRecoveredTaskEvidence, retErr error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 || before.Size() <= 0 || before.Size() > 65536 {
		return nil, storageJournalFixedError("recovered response evidence must be a bounded private regular file")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		if err := file.Close(); err != nil && retErr == nil {
			evidence = nil
			retErr = fmt.Errorf("%w: %w", storageJournalFixedError("close recovered response evidence"), err)
		}
	}()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(before, actual) || !actual.Mode().IsRegular() || actual.Mode().Perm()&0o077 != 0 || actual.Size() <= 0 || actual.Size() > 65536 {
		return nil, storageJournalFixedError("recovered response evidence changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(data) > 65536 {
		return nil, storageJournalFixedError("recovered response evidence exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result handlers.StorageRecoveredTaskEvidence
	if decoder.Decode(&result) != nil {
		return nil, storageJournalFixedError("recovered response evidence is malformed")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, storageJournalFixedError("recovered response evidence has trailing data")
	}
	return &result, nil
}
