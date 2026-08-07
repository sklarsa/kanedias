package supervisor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func TestResultCellCompletesSuccessfulReadExactlyOnce(t *testing.T) {
	cell := NewResultCell()
	want := TerminalResult{Read: &contract.ReadChildResult{Kind: contract.ChildKindRead, WorkerType: "reviewer", SessionID: "child", Output: "answer"}}
	if err := cell.Complete(want, nil); err != nil {
		t.Fatalf("Complete(read) error = %v", err)
	}
	select {
	case <-cell.Done():
	default:
		t.Fatal("Done() remains open after completion")
	}

	got, err := cell.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if got.Read == nil || *got.Read != *want.Read || got.Write != nil {
		t.Fatalf("Wait() result = %#v, want read result %#v", got, want)
	}
	if err := cell.Complete(want, nil); !errors.Is(err, ErrResultAlreadyCompleted) {
		t.Fatalf("second Complete() error = %v, want ErrResultAlreadyCompleted", err)
	}
}

func TestResultCellCompletesSuccessfulWrite(t *testing.T) {
	cell := NewResultCell()
	want := TerminalResult{Write: &contract.WriteChildResult{
		Kind: contract.ChildKindWrite, WorkerType: "worker", SessionID: "child",
		Repositories: []contract.RepositoryHandoff{{Repository: "repo", BaseCommit: "base", Branch: "branch", HeadCommit: "head"}},
		Summary:      "done", Verification: []string{"go test ./..."},
	}}
	if err := cell.Complete(want, nil); err != nil {
		t.Fatalf("Complete(write) error = %v", err)
	}
	got, err := cell.Wait(context.Background())
	if err != nil || got.Write == nil || len(got.Write.Repositories) != 1 || got.Write.Repositories[0].HeadCommit != "head" {
		t.Fatalf("Wait() = (%#v, %v), want write head", got, err)
	}
}

func TestResultCellCompletesFailureWithZeroResult(t *testing.T) {
	cell := NewResultCell()
	wantErr := errors.New("child failed")
	if err := cell.Complete(TerminalResult{}, wantErr); err != nil {
		t.Fatalf("Complete(failure) error = %v", err)
	}
	got, err := cell.Wait(context.Background())
	if !errors.Is(err, wantErr) || got.Read != nil || got.Write != nil {
		t.Fatalf("Wait() = (%#v, %v), want zero result and child failed", got, err)
	}
}

func TestResultCellRejectsInvalidCompletionWithoutConsumingCell(t *testing.T) {
	read := &contract.ReadChildResult{Kind: contract.ChildKindRead}
	write := &contract.WriteChildResult{Kind: contract.ChildKindWrite}
	tests := []struct {
		name   string
		result TerminalResult
		err    error
	}{
		{name: "success has no result"},
		{name: "success has both results", result: TerminalResult{Read: read, Write: write}},
		{name: "success read has wrong kind", result: TerminalResult{Read: &contract.ReadChildResult{Kind: contract.ChildKindWrite}}},
		{name: "failure carries result", result: TerminalResult{Read: read}, err: errors.New("failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cell := NewResultCell()
			if err := cell.Complete(tt.result, tt.err); !errors.Is(err, ErrInvariant) {
				t.Fatalf("Complete(invalid) error = %v, want ErrInvariant", err)
			}
			if err := cell.Complete(TerminalResult{}, errors.New("valid failure")); err != nil {
				t.Fatalf("Complete(valid failure) after invalid attempt error = %v", err)
			}
		})
	}
}

func TestResultCellConcurrentCompletionHasExactlyOneWinner(t *testing.T) {
	cell := NewResultCell()
	var successes atomic.Int32
	var alreadyCompleted atomic.Int32
	var unexpected atomic.Int32
	var wait sync.WaitGroup
	for n := 0; n < 32; n++ {
		wait.Add(1)
		go func(output string) {
			defer wait.Done()
			err := cell.Complete(TerminalResult{Read: &contract.ReadChildResult{Kind: contract.ChildKindRead, Output: output}}, nil)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrResultAlreadyCompleted):
				alreadyCompleted.Add(1)
			default:
				unexpected.Add(1)
			}
		}(string(rune('a' + n)))
	}
	wait.Wait()
	if successes.Load() != 1 || alreadyCompleted.Load() != 31 || unexpected.Load() != 0 {
		t.Fatalf("concurrent Complete results = successes %d, already completed %d, unexpected %d; want 1, 31, 0", successes.Load(), alreadyCompleted.Load(), unexpected.Load())
	}
}

func TestResultCellCancelledWaitDoesNotConsumeLaterCompletion(t *testing.T) {
	cell := NewResultCell()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cell.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait(cancelled) error = %v, want context.Canceled", err)
	}

	want := TerminalResult{Read: &contract.ReadChildResult{Kind: contract.ChildKindRead, Output: "later"}}
	if err := cell.Complete(want, nil); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := cell.Wait(ctx)
	if err != nil || got.Read == nil || got.Read.Output != "later" {
		t.Fatalf("second Wait() = (%#v, %v), want later result", got, err)
	}
}
