package supervisorapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
)

const (
	maxDescendantResponseBytes = 1 << 20
	maxDescendantSSELineBytes  = pirpc.MaxRecordBytes + (1 << 20)
	defaultUnaryTimeout        = 10 * time.Second
)

type cleanDescendantEventEOF struct{ err error }

func (streamErr cleanDescendantEventEOF) Error() string  { return streamErr.err.Error() }
func (streamErr cleanDescendantEventEOF) Unwrap() error  { return streamErr.err }
func (streamErr cleanDescendantEventEOF) CleanEOF() bool { return true }

type DescendantClient struct {
	socketPath   string
	client       *http.Client
	transport    *http.Transport
	unaryTimeout time.Duration
}

func NewDescendantClient(socketPath string) (supervisor.DescendantClient, error) {
	if socketPath == "" {
		return nil, contract.NewError(contract.ErrorChildUnavailable, "child socket path is empty")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &DescendantClient{socketPath: socketPath, client: &http.Client{Transport: transport}, transport: transport, unaryTimeout: defaultUnaryTimeout}, nil
}

func (client *DescendantClient) request(ctx context.Context, method, path string, body json.RawMessage) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.client.Do(request)
	if err != nil {
		return nil, contract.NewError(contract.ErrorChildUnavailable, "connect to child supervisor: "+err.Error())
	}
	return response, nil
}

func readDescendantJSON(response *http.Response, target any) error {
	defer func() { _ = response.Body.Close() }()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return contract.NewError(contract.ErrorChildUnavailable, "child response is not JSON")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDescendantResponseBytes+1))
	if err != nil {
		return contract.NewError(contract.ErrorChildUnavailable, "read child response: "+err.Error())
	}
	if len(body) > maxDescendantResponseBytes {
		return contract.NewError(contract.ErrorChildUnavailable, "child response exceeds 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var typed contract.Error
		if json.Unmarshal(body, &typed) == nil && typed.Code != "" {
			return &typed
		}
		return contract.NewError(contract.ErrorChildUnavailable, fmt.Sprintf("child returned HTTP %d", response.StatusCode))
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return contract.NewError(contract.ErrorChildUnavailable, "decode child response: "+err.Error())
	}
	return nil
}

func (client *DescendantClient) unaryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, client.unaryTimeout)
}

func (client *DescendantClient) Snapshot(ctx context.Context) (supervisor.NodeSnapshot, error) {
	ctx, cancel := client.unaryContext(ctx)
	defer cancel()
	response, err := client.request(ctx, http.MethodGet, "/v1/tree", nil)
	if err != nil {
		return supervisor.NodeSnapshot{}, err
	}
	var snapshot supervisor.NodeSnapshot
	if err := readDescendantJSON(response, &snapshot); err != nil {
		return supervisor.NodeSnapshot{}, err
	}
	return snapshot, nil
}

func (client *DescendantClient) Close() error {
	client.transport.CloseIdleConnections()
	return nil
}

func (client *DescendantClient) CallRPC(ctx context.Context, sessionID string, command json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := client.unaryContext(ctx)
	defer cancel()
	response, err := client.request(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/rpc", command)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := readDescendantJSON(response, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (client *DescendantClient) AnswerQuestion(ctx context.Context, sessionID, questionID string, answer json.RawMessage) error {
	ctx, cancel := client.unaryContext(ctx)
	defer cancel()
	response, err := client.request(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/questions/"+url.PathEscape(questionID)+"/response", answer)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	return readDescendantJSON(response, &struct{}{})
}

func (client *DescendantClient) Stop(ctx context.Context, sessionID string) error {
	ctx, cancel := client.unaryContext(ctx)
	defer cancel()
	response, err := client.request(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(sessionID), nil)
	if err != nil {
		return err
	}
	var accepted struct {
		Status string `json:"status"`
	}
	if err := readDescendantJSON(response, &accepted); err != nil {
		return err
	}
	if accepted.Status != "stopping" {
		return contract.NewError(contract.ErrorChildUnavailable, "child returned an invalid stop acknowledgement")
	}
	return nil
}

func (client *DescendantClient) Subscribe(ctx context.Context) (supervisor.Subscription, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	response, err := client.request(streamCtx, http.MethodGet, "/v1/events", nil)
	if err != nil {
		cancel()
		return supervisor.Subscription{}, err
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || mediaErr != nil || mediaType != "text/event-stream" {
		_ = response.Body.Close()
		cancel()
		return supervisor.Subscription{}, contract.NewError(contract.ErrorChildUnavailable, "child event stream is unavailable")
	}

	events := make(chan supervisor.EventEnvelope, supervisor.DefaultSubscriberMailboxCapacity)
	var closeOnce sync.Once
	var errMu sync.Mutex
	var streamErr error
	setStreamErr := func(err error) {
		errMu.Lock()
		streamErr = err
		errMu.Unlock()
	}
	closeStream := func() { closeOnce.Do(func() { cancel(); _ = response.Body.Close() }) }
	go func() {
		defer close(events)
		defer closeStream()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 64*1024), maxDescendantSSELineBytes)
		var data strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if data.Len() == 0 {
					continue
				}
				var event supervisor.EventEnvelope
				if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
					setStreamErr(contract.NewError(contract.ErrorChildUnavailable, "decode child event stream: "+err.Error()))
					return
				}
				data.Reset()
				select {
				case events <- event:
				case <-streamCtx.Done():
					return
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if streamCtx.Err() != nil {
			return
		}
		if err := scanner.Err(); err != nil {
			setStreamErr(contract.NewError(contract.ErrorChildUnavailable, "read child event stream: "+err.Error()))
			return
		}
		setStreamErr(cleanDescendantEventEOF{err: contract.NewError(contract.ErrorChildUnavailable, "child event stream ended unexpectedly")})
	}()
	return supervisor.Subscription{
		Replay: []supervisor.EventEnvelope{}, Events: events, Close: closeStream,
		Err: func() error { errMu.Lock(); defer errMu.Unlock(); return streamErr },
	}, nil
}

var _ supervisor.DescendantClient = (*DescendantClient)(nil)
