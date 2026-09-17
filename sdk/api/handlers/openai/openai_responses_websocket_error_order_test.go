package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestForwardResponsesWebsocketDrainsQueuedErrorAfterDataClose(t *testing.T) {
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	want := &interfaces.ErrorMessage{StatusCode: http.StatusUnauthorized, Error: errors.New("credential denied")}
	// Both channels are ready, so exercise both possible select orderings.
	for i := 0; i < 100; i++ {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		data := make(chan []byte)
		errs := make(chan *interfaces.ErrorMessage, 1)
		errs <- want
		close(errs)
		close(data)
		_, _, _, got, errForward := h.forwardResponsesWebsocket(ctx, nil, func(...interface{}) {}, data, errs,
			newInMemoryWebsocketTimelineLog(), "error-order", responsesWebsocketForwardOptions{
				suppressError: func(msg *interfaces.ErrorMessage) bool { return msg == want },
			})
		if got != want || errForward != nil {
			t.Fatalf("forward error = %v, %v; want queued credential error", got, errForward)
		}
	}
}
