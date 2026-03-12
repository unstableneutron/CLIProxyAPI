package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
)

func TestForwardStream_KeepAliveDeferredWhileFrameOpen(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)

	recorder, cancelCh, doneCh := startForwardStream(t, 60*time.Millisecond, data, errs, StreamForwardOptions{})

	go func() {
		data <- []byte("event: message")
		time.Sleep(130 * time.Millisecond)
		data <- []byte("data: hello")
		data <- []byte{}
		close(data)
		close(errs)
	}()

	if cancelErr := waitForwardStream(t, cancelCh, doneCh); cancelErr != nil {
		t.Fatalf("cancel err = %v, want nil", cancelErr)
	}

	body := recorder.Body.String()
	eventIdx := strings.Index(body, "event: message\n")
	dataIdx := strings.Index(body, "data: hello\n")
	keepAliveIdx := strings.Index(body, ": keep-alive\n")
	if eventIdx < 0 || dataIdx < 0 || keepAliveIdx < 0 {
		t.Fatalf("stream output missing expected parts: %q", body)
	}
	if !(eventIdx < dataIdx && dataIdx < keepAliveIdx) {
		t.Fatalf("keep-alive was not deferred until frame boundary: %q", body)
	}
	if strings.Contains(body, "event: message\n: keep-alive\n") {
		t.Fatalf("keep-alive injected mid-frame: %q", body)
	}
}

func TestForwardStream_KeepAliveEmittedBetweenCompleteFrames(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)

	recorder, cancelCh, doneCh := startForwardStream(t, 30*time.Millisecond, data, errs, StreamForwardOptions{})

	go func() {
		data <- []byte("event: one")
		data <- []byte("data: first")
		data <- []byte{}
		time.Sleep(120 * time.Millisecond)
		data <- []byte("event: two")
		data <- []byte("data: second")
		data <- []byte{}
		close(data)
		close(errs)
	}()

	if cancelErr := waitForwardStream(t, cancelCh, doneCh); cancelErr != nil {
		t.Fatalf("cancel err = %v, want nil", cancelErr)
	}

	body := recorder.Body.String()
	frame1End := strings.Index(body, "data: first\n\n")
	frame2Start := strings.Index(body, "event: two\n")
	keepAliveIdx := strings.Index(body, ": keep-alive\n")
	if frame1End < 0 || frame2Start < 0 || keepAliveIdx < 0 {
		t.Fatalf("stream output missing expected parts: %q", body)
	}
	frame1End += len("data: first\n\n")
	if keepAliveIdx < frame1End || keepAliveIdx > frame2Start {
		t.Fatalf("keep-alive should be between complete frames, got: %q", body)
	}
}

func TestForwardStream_NoKeepAliveWhenIntervalDisabled(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	var keepAliveCalls int32

	opts := StreamForwardOptions{
		WriteKeepAlive: func() {
			atomic.AddInt32(&keepAliveCalls, 1)
		},
	}

	recorder, cancelCh, doneCh := startForwardStream(t, 0, data, errs, opts)

	go func() {
		data <- []byte("event: done")
		data <- []byte("data: ok")
		data <- []byte{}
		close(data)
		close(errs)
	}()

	if cancelErr := waitForwardStream(t, cancelCh, doneCh); cancelErr != nil {
		t.Fatalf("cancel err = %v, want nil", cancelErr)
	}
	if got := atomic.LoadInt32(&keepAliveCalls); got != 0 {
		t.Fatalf("keep-alive calls = %d, want 0", got)
	}
	if strings.Contains(recorder.Body.String(), ": keep-alive") {
		t.Fatalf("unexpected keep-alive output when interval disabled: %q", recorder.Body.String())
	}
}

func TestForwardStream_TerminalErrorWhileFrameOpen(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	streamErr := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("upstream failed")}
	var keepAliveCalls int32
	var terminalCalls int32

	_, cancelCh, doneCh := startForwardStream(t, 15*time.Millisecond, data, errs, StreamForwardOptions{
		WriteKeepAlive: func() {
			atomic.AddInt32(&keepAliveCalls, 1)
		},
		WriteTerminalError: func(_ *interfaces.ErrorMessage) {
			atomic.AddInt32(&terminalCalls, 1)
		},
	})

	go func() {
		data <- []byte("event: partial")
		time.Sleep(30 * time.Millisecond)
		errs <- streamErr
		close(data)
		close(errs)
	}()

	cancelErr := waitForwardStream(t, cancelCh, doneCh)
	if !errors.Is(cancelErr, streamErr.Error) {
		t.Fatalf("cancel err = %v, want %v", cancelErr, streamErr.Error)
	}
	if got := atomic.LoadInt32(&keepAliveCalls); got != 0 {
		t.Fatalf("keep-alive calls during open frame error = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&terminalCalls); got != 1 {
		t.Fatalf("terminal error calls = %d, want 1", got)
	}
}

func TestForwardStream_CloseWhileFrameOpenPrefersPendingTerminalError(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	streamErr := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("upstream failed")}
	var terminalCalls int32
	var recorder *httptest.ResponseRecorder

	recorder, cancelCh, doneCh := startForwardStream(t, 0, data, errs, StreamForwardOptions{
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			atomic.AddInt32(&terminalCalls, 1)
			_, _ = recorder.WriteString("terminal:" + errMsg.Error.Error() + "\n")
		},
	})

	go func() {
		data <- []byte("event: partial")
		errs <- streamErr
		close(data)
		close(errs)
	}()

	cancelErr := waitForwardStream(t, cancelCh, doneCh)
	if !errors.Is(cancelErr, streamErr.Error) {
		t.Fatalf("cancel err = %v, want %v", cancelErr, streamErr.Error)
	}
	if got := atomic.LoadInt32(&terminalCalls); got != 1 {
		t.Fatalf("terminal error calls = %d, want 1", got)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "\n\nterminal:upstream failed\n") {
		t.Fatalf("terminal error should be emitted as a distinct block: %q", body)
	}
}

func TestForwardStream_CloseWhileFrameOpenSeparatesWriteDone(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	var doneCalls int32
	var recorder *httptest.ResponseRecorder

	recorder, cancelCh, doneCh := startForwardStream(t, 0, data, errs, StreamForwardOptions{
		WriteDone: func() {
			atomic.AddInt32(&doneCalls, 1)
			_, _ = recorder.WriteString("data: [DONE]\n\n")
		},
	})

	go func() {
		data <- []byte("event: partial")
		close(data)
		close(errs)
	}()

	if cancelErr := waitForwardStream(t, cancelCh, doneCh); cancelErr != nil {
		t.Fatalf("cancel err = %v, want nil", cancelErr)
	}
	if got := atomic.LoadInt32(&doneCalls); got != 1 {
		t.Fatalf("done calls = %d, want 1", got)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "event: partial\ndata: [DONE]\n\n") {
		t.Fatalf("done marker merged into open SSE frame: %q", body)
	}
	if !strings.Contains(body, "\n\ndata: [DONE]\n\n") {
		t.Fatalf("done marker should be emitted as a distinct block: %q", body)
	}
}

func TestForwardStream_CloseWhileFrameOpenWithoutWriteDoneClosesFrame(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)

	recorder, cancelCh, doneCh := startForwardStream(t, 0, data, errs, StreamForwardOptions{})

	go func() {
		data <- []byte("event: partial")
		close(data)
		close(errs)
	}()

	if cancelErr := waitForwardStream(t, cancelCh, doneCh); cancelErr != nil {
		t.Fatalf("cancel err = %v, want nil", cancelErr)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: partial\n\n") {
		t.Fatalf("open frame should be terminated on clean EOF: %q", body)
	}
}

func startForwardStream(t *testing.T, keepAliveInterval time.Duration, data <-chan []byte, errs <-chan *interfaces.ErrorMessage, opts StreamForwardOptions) (*httptest.ResponseRecorder, <-chan error, <-chan struct{}) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	if opts.WriteChunk == nil {
		opts.WriteChunk = func(chunk []byte) {
			_, _ = c.Writer.Write(append(chunk, '\n'))
		}
	}
	if opts.WriteKeepAlive == nil {
		opts.WriteKeepAlive = func() {
			_, _ = c.Writer.Write([]byte(": keep-alive\n"))
		}
	}
	interval := keepAliveInterval
	opts.KeepAliveInterval = &interval

	cancelCh := make(chan error, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		handler.ForwardStream(c, recorder, func(err error) {
			cancelCh <- err
		}, data, errs, opts)
	}()

	return recorder, cancelCh, doneCh
}

func waitForwardStream(t *testing.T, cancelCh <-chan error, doneCh <-chan struct{}) error {
	t.Helper()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("ForwardStream did not complete")
	}

	select {
	case err := <-cancelCh:
		return err
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ForwardStream returned without invoking cancel")
	}

	return nil
}
