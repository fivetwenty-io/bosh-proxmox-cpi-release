//go:build !linux

package main

// bpmWorkerView looks inside a running BPM worker, which exists only on Linux,
// so elsewhere no worker can be looked into.
func bpmWorkerView(string) func(parent, anchor string) (workerMount, error) { return nil }
