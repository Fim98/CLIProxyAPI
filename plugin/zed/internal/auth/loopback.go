package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxCallbackParamLen = 64 << 10
	// loopbackShutdownGrace lets the browser receive the redirect page while
	// keeping every teardown path bounded.
	loopbackShutdownGrace = time.Second
)

// callbackResult is the payload captured from the browser redirect.
type callbackResult struct {
	userID      string
	accessToken string
	errorText   string
}

// loopbackCapture owns a single-use HTTP listener bound to 127.0.0.1 that
// receives exactly one Zed sign-in callback.
//
// Zed's native app sign-in redirects the browser to
// http://127.0.0.1:{port}/?user_id=...&access_token={rsa-encrypted}, so the
// callback can never travel through CPA's /v0/management/oauth-callback
// endpoint: it carries no OAuth code. The listener mirrors the Zed desktop
// client's tiny_http server, including the 302 answer.
type loopbackCapture struct {
	// listenerAddr records "127.0.0.1:port" of the bound listener so the
	// sign-in URL can name the actual port after an ephemeral bind.
	listenerAddr string
	results      chan callbackResult
	server       *http.Server
	done         chan struct{}
	closeOnce    sync.Once
}

func listenerPort(capture *loopbackCapture) int {
	_, portText, errSplit := net.SplitHostPort(capture.listenerAddr)
	if errSplit != nil {
		return 0
	}
	port, errParse := strconv.Atoi(portText)
	if errParse != nil {
		return 0
	}
	return port
}

// startLoopbackCapture binds the listener before any sign-in URL is handed
// out. A port of 0 takes an ephemeral port.
func startLoopbackCapture(port int, successURL string) (*loopbackCapture, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid Zed OAuth callback port")
	}
	listener, errListen := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if errListen != nil {
		return nil, fmt.Errorf("start Zed sign-in callback listener: %w", errListen)
	}
	capture := &loopbackCapture{
		listenerAddr: listener.Addr().String(),
		results:      make(chan callbackResult, 1),
		done:         make(chan struct{}),
	}
	capture.server = &http.Server{
		Handler:           zedCallbackHandler(successURL, capture.results),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if errServe := capture.server.Serve(listener); errServe != nil && errServe != http.ErrServerClosed {
			select {
			case capture.results <- callbackResult{errorText: "Zed sign-in callback listener stopped"}:
			default:
			}
		}
	}()
	return capture, nil
}

// Results yields the one captured callback.
func (c *loopbackCapture) Results() <-chan callbackResult {
	return c.results
}

// Done closes when the listener is being torn down.
func (c *loopbackCapture) Done() <-chan struct{} {
	return c.done
}

// Close releases the port.
func (c *loopbackCapture) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		close(c.done)
		ctx, cancel := context.WithTimeout(context.Background(), loopbackShutdownGrace)
		defer cancel()
		if errShutdown := c.server.Shutdown(ctx); errShutdown != nil {
			_ = c.server.Close()
		}
	})
}

// zedCallbackHandler accepts the first callback on any path, mirrors the
// desktop client's 302 to /native_app_signin_succeeded, and forwards the
// captured parameters exactly once.
func zedCallbackHandler(successURL string, results chan<- callbackResult) http.Handler {
	var used atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !strings.EqualFold(r.Method, http.MethodGet) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		values := r.URL.Query()
		result := callbackResult{
			userID:      strings.TrimSpace(values.Get("user_id")),
			accessToken: strings.TrimSpace(values.Get("access_token")),
		}
		if strings.TrimSpace(values.Get("error")) != "" {
			result.errorText = "Zed cancelled or rejected the sign-in"
		}
		if result.errorText == "" && (result.userID == "" || result.accessToken == "") {
			result.errorText = "callback did not include sign-in credentials"
		}
		if len(result.userID) > maxCallbackParamLen || len(result.accessToken) > maxCallbackParamLen {
			result.errorText = "callback credentials exceeded the accepted size"
		}
		if !used.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		select {
		case results <- result:
		default:
		}
		// The desktop client always redirects the browser to the success page
		// once the callback arrives; the credential is validated afterwards.
		w.Header().Set("Location", successURL)
		w.WriteHeader(http.StatusFound)
	})
	return mux
}
