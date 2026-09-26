package auth

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// commandLineHTTPClient backs the --zed-login flow, which runs before any
// host request context exists and therefore cannot borrow the host's HTTP
// client. It serves profile validation only, so streaming is never needed.
type commandLineHTTPClient struct {
	inner *http.Client
}

func newCommandLineHTTPClient() commandLineHTTPClient {
	return commandLineHTTPClient{inner: &http.Client{}}
}

func (c commandLineHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	httpReq, errNew := http.NewRequestWithContext(context.Background(), req.Method, req.URL, bytes.NewReader(req.Body))
	if errNew != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("build Zed request: %w", errNew)
	}
	for key, values := range req.Headers {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	httpResp, errDo := c.inner.Do(httpReq)
	if errDo != nil {
		return pluginapi.HTTPResponse{}, errDo
	}
	defer func() { _ = httpResp.Body.Close() }()
	body, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("read Zed response: %w", errRead)
	}
	return pluginapi.HTTPResponse{StatusCode: httpResp.StatusCode, Headers: httpResp.Header, Body: body}, nil
}

func (c commandLineHTTPClient) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	response, errDo := c.Do(ctx, req)
	if errDo != nil {
		return pluginapi.HTTPStreamResponse{}, errDo
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, 1)
	chunks <- pluginapi.HTTPStreamChunk{Payload: response.Body}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: response.StatusCode, Headers: response.Headers, Chunks: chunks}, nil
}
