package supervisorapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sklarsa/kanedias/internal/eventmailbox"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
)

const (
	maxDescendantResponseBytes = 1 << 20
	maxDescendantSSELineBytes  = pirpc.MaxRecordBytes + (1 << 20)
	maxDescendantSSEEventBytes = maxDescendantSSELineBytes
	defaultUnaryTimeout        = 10 * time.Second
)

type cleanDescendantEventEOF struct{ err error }

func (streamErr cleanDescendantEventEOF) Error() string { return streamErr.err.Error() }
func (streamErr cleanDescendantEventEOF) Unwrap() error { return streamErr.err }

type DescendantClient struct {
	socketPath   string
	client       *http.Client
	transport    *http.Transport
	unaryTimeout time.Duration
	eventLimits  eventmailbox.Limits
}

// NewClient constructs a concrete *DescendantClient for the manager's private
// rootClient seam. Callers that only need supervisor.DescendantClient should
// use NewDescendantClient.
func NewClient(socketPath string) (*DescendantClient, error) {
	if socketPath == "" {
		return nil, contract.NewError(contract.ErrorChildUnavailable, "child socket path is empty")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &DescendantClient{
		socketPath:   socketPath,
		client:       &http.Client{Transport: transport},
		transport:    transport,
		unaryTimeout: defaultUnaryTimeout,
		eventLimits: eventmailbox.Limits{
			MaxEvents: supervisor.DefaultEventRingCapacity,
			MaxBytes:  supervisor.DefaultEventRingByteCapacity,
		},
	}, nil
}

func NewDescendantClient(socketPath string) (supervisor.DescendantClient, error) {
	return NewClient(socketPath)
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

func readDescendantJSON(response *http.Response, target any, maxBytes int64) error {
	defer func() { _ = response.Body.Close() }()
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return contract.NewError(contract.ErrorChildUnavailable, "child response is not JSON")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return contract.NewError(contract.ErrorChildUnavailable, "read child response: "+err.Error())
	}
	if int64(len(body)) > maxBytes {
		if maxBytes == maxDescendantResponseBytes {
			return contract.NewError(contract.ErrorChildUnavailable, "child response exceeds 1 MiB")
		}
		return contract.NewError(contract.ErrorChildUnavailable, fmt.Sprintf("child response exceeds %d bytes", maxBytes))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var typed contract.Error
		if json.Unmarshal(body, &typed) == nil && typed.Code != "" {
			return &typed
		}
		return contract.NewError(contract.ErrorChildUnavailable, fmt.Sprintf("child returned HTTP %d", response.StatusCode))
	}
	if err := json.Unmarshal(body, target); err != nil {
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
	if err := readDescendantJSON(response, &snapshot, maxDescendantResponseBytes); err != nil {
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
	if err := readDescendantJSON(response, &raw, pirpc.MaxRecordBytes); err != nil {
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
	return readDescendantJSON(response, &struct{}{}, maxDescendantResponseBytes)
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
	if err := readDescendantJSON(response, &accepted, maxDescendantResponseBytes); err != nil {
		return err
	}
	if accepted.Status != "stopping" {
		return contract.NewError(contract.ErrorChildUnavailable, "child returned an invalid stop acknowledgement")
	}
	return nil
}

func appendDescendantSSEData(data *strings.Builder, value string, maxBytes int) error {
	additional := len(value)
	if data.Len() > 0 {
		additional++
	}
	if additional > maxBytes-data.Len() {
		return fmt.Errorf("child event data exceeds %d bytes", maxBytes)
	}
	if data.Len() > 0 {
		data.WriteByte('\n')
	}
	data.WriteString(value)
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

	mailbox, err := eventmailbox.New[supervisor.EventEnvelope](client.eventLimits)
	if err != nil {
		_ = response.Body.Close()
		cancel()
		return supervisor.Subscription{}, contract.NewError(contract.ErrorChildUnavailable, "configure child event mailbox: "+err.Error())
	}

	var networkCloseOnce sync.Once
	closeNetwork := func() {
		networkCloseOnce.Do(func() {
			cancel()
			_ = response.Body.Close()
		})
	}
	var errMu sync.Mutex
	var streamErr error
	ownerCanceled := false
	finishWire := func(err error, abort bool) {
		errMu.Lock()
		ownerWon := ownerCanceled || ctx.Err() != nil
		if !ownerWon {
			streamErr = err
		}
		errMu.Unlock()

		closeNetwork()
		if ownerWon || abort {
			mailbox.Abort()
			return
		}
		mailbox.Close()
	}
	var subscriptionCloseOnce sync.Once
	closeSubscription := func() {
		subscriptionCloseOnce.Do(func() {
			errMu.Lock()
			ownerCanceled = true
			errMu.Unlock()
			closeNetwork()
			mailbox.Abort()
		})
	}
	go func() {
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
					finishWire(contract.NewError(contract.ErrorChildUnavailable, "decode child event stream: "+err.Error()), false)
					return
				}
				data.Reset()
				if err := mailbox.Send(event, supervisor.RetainedEventBytes(event)); err != nil {
					if errors.Is(err, eventmailbox.ErrFull) {
						finishWire(contract.NewError(contract.ErrorChildUnavailable, "child event consumer exceeded bounded capacity"), true)
					}
					return
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if err := appendDescendantSSEData(&data, value, maxDescendantSSEEventBytes); err != nil {
					finishWire(contract.NewError(contract.ErrorChildUnavailable, err.Error()), false)
					return
				}
			}
		}
		if streamCtx.Err() != nil {
			closeNetwork()
			mailbox.Abort()
			return
		}
		if err := scanner.Err(); err != nil {
			finishWire(contract.NewError(contract.ErrorChildUnavailable, "read child event stream: "+err.Error()), false)
			return
		}
		finishWire(cleanDescendantEventEOF{err: contract.NewError(contract.ErrorChildUnavailable, "child event stream ended unexpectedly")}, false)
	}()
	return supervisor.Subscription{
		Replay: []supervisor.EventEnvelope{}, Events: mailbox.Events(), Close: closeSubscription,
		Err: func() error { errMu.Lock(); defer errMu.Unlock(); return streamErr },
	}, nil
}

var _ supervisor.DescendantClient = (*DescendantClient)(nil)
