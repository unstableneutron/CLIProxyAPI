package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
)

func isSSEFrameBoundary(chunk []byte) bool {
	return len(chunk) == 0
}

type StreamForwardOptions struct {
	// KeepAliveInterval overrides the configured streaming keep-alive interval.
	// If nil, the configured default is used. If set to <= 0, keep-alives are disabled.
	KeepAliveInterval *time.Duration

	// WriteChunk writes a single data chunk to the response body. It should not flush.
	WriteChunk func(chunk []byte)

	// WriteTerminalError writes an error payload to the response body when streaming fails
	// after headers have already been committed. It should not flush.
	WriteTerminalError func(errMsg *interfaces.ErrorMessage)

	// WriteDone optionally writes a terminal marker when the upstream data channel closes
	// without an error (e.g. OpenAI's `[DONE]`). It should not flush.
	WriteDone func()

	// WriteKeepAlive optionally writes a keep-alive heartbeat. It should not flush.
	// When nil, a standard SSE comment heartbeat is used.
	WriteKeepAlive func()
}

func (h *BaseAPIHandler) ForwardStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, opts StreamForwardOptions) {
	if c == nil {
		return
	}
	if cancel == nil {
		return
	}

	writeChunk := opts.WriteChunk
	if writeChunk == nil {
		writeChunk = func(chunk []byte) {
			_, _ = c.Writer.Write(chunk)
			_, _ = c.Writer.Write([]byte("\n"))
		}
	}

	writeKeepAlive := opts.WriteKeepAlive
	if writeKeepAlive == nil {
		writeKeepAlive = func() {
			_, _ = c.Writer.Write([]byte(": keep-alive\n"))
		}
	}

	keepAliveInterval := StreamingKeepAliveInterval(h.Cfg)
	if opts.KeepAliveInterval != nil {
		keepAliveInterval = *opts.KeepAliveInterval
	}
	var keepAlive *time.Ticker
	var keepAliveC <-chan time.Time
	if keepAliveInterval > 0 {
		keepAlive = time.NewTicker(keepAliveInterval)
		defer keepAlive.Stop()
		keepAliveC = keepAlive.C
	}
	var terminalErr *interfaces.ErrorMessage
	dataC := data
	errsC := errs

	emitTerminalError := func(errMsg *interfaces.ErrorMessage, frameOpen *bool, pendingKeepAlive *bool) {
		if errMsg == nil || opts.WriteTerminalError == nil {
			return
		}
		if *frameOpen {
			_, _ = c.Writer.Write([]byte("\n\n"))
			flusher.Flush()
			*frameOpen = false
			*pendingKeepAlive = false
		}
		opts.WriteTerminalError(errMsg)
		flusher.Flush()
	}

	readTerminalError := func() {
		if terminalErr != nil || errsC == nil {
			return
		}
		for {
			select {
			case errMsg, ok := <-errsC:
				if !ok {
					errsC = nil
					return
				}
				if errMsg != nil {
					terminalErr = errMsg
					return
				}
			default:
				return
			}
		}
	}

	emitDone := func(frameOpen *bool, pendingKeepAlive *bool) {
		if *frameOpen {
			_, _ = c.Writer.Write([]byte("\n\n"))
			*frameOpen = false
			*pendingKeepAlive = false
		}
		if opts.WriteDone != nil {
			opts.WriteDone()
		}
		flusher.Flush()
		cancel(nil)
	}

	var frameOpen bool
	var pendingKeepAlive bool
	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case chunk, ok := <-dataC:
			if !ok {
				dataC = nil
				keepAliveC = nil
				// Wait for errs to deliver terminal status or close before declaring success.
				readTerminalError()
				if terminalErr != nil {
					emitTerminalError(terminalErr, &frameOpen, &pendingKeepAlive)
					cancel(terminalErr.Error)
					return
				}
				if errsC == nil {
					emitDone(&frameOpen, &pendingKeepAlive)
					return
				}
				continue
			}
			writeChunk(chunk)
			flusher.Flush()
			if isSSEFrameBoundary(chunk) {
				frameOpen = false
				if pendingKeepAlive {
					writeKeepAlive()
					flusher.Flush()
					pendingKeepAlive = false
				}
				continue
			}
			frameOpen = true
		case errMsg, ok := <-errsC:
			if !ok {
				errsC = nil
				if dataC == nil {
					emitDone(&frameOpen, &pendingKeepAlive)
					return
				}
				continue
			}
			if errMsg == nil {
				continue
			}
			terminalErr = errMsg
			emitTerminalError(errMsg, &frameOpen, &pendingKeepAlive)
			cancel(errMsg.Error)
			return
		case <-keepAliveC:
			if frameOpen {
				pendingKeepAlive = true
				continue
			}
			writeKeepAlive()
			flusher.Flush()
		}
	}
}
