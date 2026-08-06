package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

const promptRequestID = "prompt-1"

type promptCommand struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

type rpcEnvelope struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Success *bool  `json:"success,omitempty"`
	Error   string `json:"error,omitempty"`
}

func runRPC(ctx context.Context, conn net.Conn, prompt string, stdout io.Writer) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	command := promptCommand{
		ID:      promptRequestID,
		Type:    "prompt",
		Message: prompt,
	}
	if err := json.NewEncoder(conn).Encode(command); err != nil {
		return fmt.Errorf("send prompt command: %w", err)
	}

	reader := bufio.NewReader(conn)
	for {
		record, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				if len(record) != 0 {
					return fmt.Errorf("read RPC record: partial record before EOF")
				}
				return fmt.Errorf("RPC stream ended before agent settled: %w", err)
			}
			return fmt.Errorf("read RPC record: %w", err)
		}

		var envelope rpcEnvelope
		if err := json.Unmarshal(record, &envelope); err != nil {
			return fmt.Errorf("decode RPC record: %w", err)
		}
		if err := writeRecord(stdout, record); err != nil {
			return fmt.Errorf("forward RPC record: %w", err)
		}

		if envelope.Type == "response" && envelope.Command == "prompt" &&
			envelope.Success != nil && !*envelope.Success {
			return fmt.Errorf("prompt RPC failed: %s", envelope.Error)
		}
		if envelope.Type == "agent_settled" {
			return nil
		}
	}
}

func writeRecord(writer io.Writer, record []byte) error {
	n, err := writer.Write(record)
	if err != nil {
		return err
	}
	if n != len(record) {
		return io.ErrShortWrite
	}
	return nil
}
