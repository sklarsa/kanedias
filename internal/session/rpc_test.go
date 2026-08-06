package session

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
)

func TestRunRPCForwardsStreamUntilAgentSettled(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const records = "{\"id\":\"prompt-1\",\"type\":\"response\",\"command\":\"prompt\",\"success\":true}\n" +
		"{\"type\":\"message_update\",\"assistantMessageEvent\":{\"type\":\"text_delta\",\"delta\":\"hello\"}}\n" +
		"{\"type\":\"agent_settled\"}\n"

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

		var command promptCommand
		if err := json.NewDecoder(server).Decode(&command); err != nil {
			t.Errorf("decode prompt command: %v", err)
			return
		}
		if command.Type != "prompt" || command.ID != "prompt-1" ||
			command.Message != "first line\nsecond line\n" {
			t.Errorf("command = %#v", command)
		}
		if _, err := server.Write([]byte(records)); err != nil {
			t.Errorf("write records: %v", err)
		}
	}()

	var stdout bytes.Buffer
	if err := runRPC(context.Background(), client, "first line\nsecond line\n", &stdout); err != nil {
		t.Fatalf("runRPC: %v", err)
	}
	<-serverDone

	if stdout.String() != records {
		t.Errorf("stdout = %q, want %q", stdout.String(), records)
	}
}

func TestRunRPCReturnsFailedPromptResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const record = "{\"id\":\"prompt-1\",\"type\":\"response\",\"command\":\"prompt\",\"success\":false,\"error\":\"prompt rejected\"}\n"
	go func() {
		var command promptCommand
		if err := json.NewDecoder(server).Decode(&command); err != nil {
			t.Errorf("decode prompt command: %v", err)
			return
		}
		if _, err := server.Write([]byte(record)); err != nil {
			t.Errorf("write record: %v", err)
		}
	}()

	var stdout bytes.Buffer
	err := runRPC(context.Background(), client, "hello", &stdout)
	if err == nil || !strings.Contains(err.Error(), "prompt rejected") {
		t.Fatalf("runRPC error = %v, want remote error text", err)
	}
	if stdout.String() != record {
		t.Errorf("stdout = %q, want %q", stdout.String(), record)
	}
}

func TestRunRPCRejectsEOFBeforeAgentSettled(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	go func() {
		defer server.Close()
		var command promptCommand
		if err := json.NewDecoder(server).Decode(&command); err != nil {
			t.Errorf("decode prompt command: %v", err)
			return
		}
		const record = "{\"id\":\"prompt-1\",\"type\":\"response\",\"command\":\"prompt\",\"success\":true}\n"
		if _, err := server.Write([]byte(record)); err != nil {
			t.Errorf("write record: %v", err)
		}
	}()

	var stdout bytes.Buffer
	if err := runRPC(context.Background(), client, "hello", &stdout); err == nil {
		t.Fatal("runRPC error = nil, want EOF before settlement error")
	}
}
