package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func TestRunWriteTaskPromptsExactlyAndLeavesWriterLiveUntilHandoff(t *testing.T) {
	identity := writerIdentity(t)
	local, peer, broker := newTestLocalSession(t, identity)
	bindTestLocal(t, local, peer, "pi-writer", "/tmp/writer.jsonl", false)
	node := &Node{identity: identity, local: local, broker: broker, state: LifecycleReady}

	done := make(chan error, 1)
	go func() { done <- node.RunWriteTask(context.Background(), "implement exactly") }()
	line, err := bufio.NewReader(peer).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var prompt struct{ ID, Type, Message string }
	if err := json.Unmarshal(line, &prompt); err != nil {
		t.Fatal(err)
	}
	if prompt.Type != "prompt" || prompt.Message != "implement exactly" {
		t.Fatalf("prompt = %#v", prompt)
	}
	writeLine(t, peer, `{"id":"`+prompt.ID+`","type":"response","command":"prompt","success":true}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := local.Snapshot().Lifecycle; got != string(LifecycleRunning) {
		t.Fatalf("lifecycle = %s", got)
	}
	writeLine(t, peer, `{"type":"agent_settled"}`)
	waitFor(t, func() bool { return local.Snapshot().Lifecycle == string(LifecycleAwaitingHandoff) }, "writer to remain awaiting handoff")
	node.reportWrite = func(contract.WriteChildResult) error { return nil }
	_, err = node.Handoff(context.Background(), WriteHandoffRequest{
		Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "feature", HeadCommit: "head"}},
		Summary:      "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := local.Snapshot().Lifecycle; got != string(LifecycleCompleted) {
		t.Fatalf("lifecycle after handoff = %s", got)
	}
}

func TestWriterHandoffRecordsAndForwardsExactlyOnceBeforeAcceptance(t *testing.T) {
	identity, err := NewIdentity(IdentitySpec{SessionID: "writer-1", ParentID: "root-1", RootID: "root-1", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var forwarded []contract.WriteChildResult
	node := &Node{identity: identity, reportWrite: func(result contract.WriteChildResult) error {
		mu.Lock()
		defer mu.Unlock()
		forwarded = append(forwarded, result)
		return nil
	}}
	request := WriteHandoffRequest{
		Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "feature", HeadCommit: "head"}},
		Summary:      "done", Verification: []string{"go test ./..."},
	}
	accepted, err := node.Handoff(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || accepted.SessionID != "writer-1" {
		t.Fatalf("accepted = %#v", accepted)
	}
	mu.Lock()
	if len(forwarded) != 1 {
		t.Fatalf("forwarded = %#v", forwarded)
	}
	got := forwarded[0]
	mu.Unlock()
	if got.SessionID != "writer-1" || got.WorkerType != "worker" || got.Kind != contract.ChildKindWrite {
		t.Fatalf("result identity = %#v", got)
	}
	if got.Repositories[0].Repository != "owner/repo" {
		t.Fatalf("repositories = %#v", got.Repositories)
	}

	_, err = node.Handoff(context.Background(), request)
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorConflict {
		t.Fatalf("duplicate error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(forwarded) != 1 {
		t.Fatalf("duplicate forwarded %d results", len(forwarded))
	}
}

func TestRejectedHandoffLeavesWriterLiveAndRetryable(t *testing.T) {
	identity, err := NewIdentity(IdentitySpec{SessionID: "writer-1", ParentID: "root-1", RootID: "root-1", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	node := &Node{identity: identity, reportWrite: func(contract.WriteChildResult) error {
		attempts++
		if attempts == 1 {
			return errors.New("report pipe unavailable")
		}
		return nil
	}}
	request := WriteHandoffRequest{Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "feature", HeadCommit: "head"}}, Summary: "done"}
	if _, err := node.Handoff(context.Background(), request); err == nil {
		t.Fatal("expected rejected handoff")
	}
	if node.handoffAccepted() {
		t.Fatal("rejected handoff became terminal")
	}
	if _, err := node.Handoff(context.Background(), request); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestHandoffRejectsRootReadEmptyAndDuplicateRepositories(t *testing.T) {
	for _, kind := range []contract.ChildKind{contract.ChildKindRoot, contract.ChildKindRead} {
		parent, root := "root-1", "root-1"
		contextMode := contract.ContextRoot
		worker := ""
		if kind == contract.ChildKindRead {
			parent, contextMode, worker = "root-1", contract.ContextFresh, "reviewer"
		}
		identity, err := NewIdentity(IdentitySpec{SessionID: map[bool]string{true: "root-1", false: "read-1"}[kind == contract.ChildKindRoot], ParentID: map[bool]string{true: "", false: parent}[kind == contract.ChildKindRoot], RootID: root, Kind: kind, Context: contextMode, Worker: worker})
		if err != nil {
			t.Fatal(err)
		}
		node := &Node{identity: identity, reportWrite: func(contract.WriteChildResult) error { t.Fatal("must not report"); return nil }}
		_, err = node.Handoff(context.Background(), WriteHandoffRequest{Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "branch", HeadCommit: "head"}}, Summary: "done"})
		var typed *contract.Error
		if !errors.As(err, &typed) || typed.Code != contract.ErrorConflict {
			t.Fatalf("%s error = %v", kind, err)
		}
	}

	identity, _ := NewIdentity(IdentitySpec{SessionID: "writer-1", ParentID: "root-1", RootID: "root-1", Kind: contract.ChildKindWrite, Context: contract.ContextFresh, Worker: "worker"})
	node := &Node{identity: identity, reportWrite: func(contract.WriteChildResult) error { t.Fatal("invalid request forwarded"); return nil }}
	invalid := []WriteHandoffRequest{
		{Summary: "done"},
		{Repositories: []contract.RepositoryHandoff{{Repository: "owner/repo", BaseCommit: "base", Branch: "one", HeadCommit: "head"}, {Repository: "owner/repo", BaseCommit: "base", Branch: "two", HeadCommit: "head"}}, Summary: "done"},
	}
	for _, request := range invalid {
		if _, err := node.Handoff(context.Background(), request); err == nil {
			t.Fatalf("accepted invalid request %#v", request)
		}
	}
}
