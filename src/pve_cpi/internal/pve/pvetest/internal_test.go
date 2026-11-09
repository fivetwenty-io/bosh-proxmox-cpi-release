package pvetest

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// failTB records a failure message and unwinds with a panic, so a test can read
// what a helper reported to the test it belongs to.
type failTB struct {
	testing.TB
	failed atomic.Bool
	msg    atomic.Value
}

func (f *failTB) Fatalf(format string, args ...any) {
	f.failed.Store(true)
	f.msg.Store(fmt.Sprintf(format, args...))
	panic(f)
}

func TestWriteRouted_HeadWritesLengthAndNoBody(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req := httptest.NewRequestWithContext(context.Background(), method, APIPrefix+"/version", nil)
		var out bytes.Buffer
		if err := writeRouted(&out, NewRoutes(), req); err != nil {
			t.Fatalf("%s: writeRouted: %v", method, err)
		}
		br := bufio.NewReader(&out)
		resp, err := http.ReadResponse(br, req)
		if err != nil {
			t.Fatalf("%s: read response: %v", method, err)
		}
		rest := br.Buffered() + out.Len()
		if resp.ContentLength <= 0 {
			t.Fatalf("%s: Content-Length = %d, want the body length", method, resp.ContentLength)
		}
		if method == http.MethodHead && rest != 0 {
			t.Fatalf("HEAD answer left %d unread bytes after the headers", rest)
		}
	}
}

func TestEmit_CountsDroppedEventsAndWaitNamesThem(t *testing.T) {
	t.Parallel()
	s := newServer(t, KindLive, listenLoopback(t), nil)

	for range eventBuffer + 3 {
		s.emit(EventRequest, Event{})
	}
	if got := s.Dropped(EventRequest); got != 3 {
		t.Fatalf("Dropped(EventRequest) = %d, want 3", got)
	}
	if s.Dropped(EventConn) != 0 || s.Dropped(EventKind(-1)) != 0 || s.Dropped(eventKinds) != 0 {
		t.Fatal("other kinds and out-of-range kinds must report no drops")
	}

	s.Close()
	ft := &failTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		s.Wait(ft, EventConn)
	}()
	msg, _ := ft.msg.Load().(string)
	if !ft.failed.Load() || !strings.Contains(msg, "closed while waiting") || !strings.Contains(msg, "0 events") {
		t.Fatalf("Wait failure = %q", msg)
	}

	// A closed server with a full queue reports its drops in the failure.
	s2 := newServer(t, KindLive, listenLoopback(t), nil)
	for range eventBuffer + 2 {
		s2.emit(EventDial, Event{})
	}
	for range eventBuffer {
		s2.Wait(t, EventDial)
	}
	s2.Close()
	ft2 := &failTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		s2.Wait(ft2, EventDial)
	}()
	msg, _ = ft2.msg.Load().(string)
	if !strings.Contains(msg, "(2 events of that kind dropped)") {
		t.Fatalf("Wait failure = %q, want the drop count", msg)
	}
}
