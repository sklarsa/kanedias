package supervisor

import (
	"context"
	"errors"
	"sync"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

var ErrResultAlreadyCompleted = errors.New("result cell is already completed")

type TerminalResult struct {
	Read  *contract.ReadChildResult
	Write *contract.WriteChildResult
}

func (result TerminalResult) validate(completionErr error) error {
	if completionErr != nil {
		if result.Read != nil || result.Write != nil {
			return invariantf("failed completion must not carry a terminal result")
		}
		return nil
	}
	if (result.Read == nil) == (result.Write == nil) {
		return invariantf("successful completion must carry exactly one terminal result")
	}
	if result.Read != nil {
		if err := result.Read.Validate(); err != nil {
			return invariantf("invalid read terminal result: %v", err)
		}
		return nil
	}
	if err := result.Write.Validate(); err != nil {
		return invariantf("invalid write terminal result: %v", err)
	}
	return nil
}

type ResultCell struct {
	mu        sync.Mutex
	done      chan struct{}
	completed bool
	result    TerminalResult
	err       error
}

func NewResultCell() *ResultCell {
	return &ResultCell{done: make(chan struct{})}
}

func (cell *ResultCell) Complete(result TerminalResult, completionErr error) error {
	cell.mu.Lock()
	if cell.completed {
		cell.mu.Unlock()
		return ErrResultAlreadyCompleted
	}
	cell.mu.Unlock()

	if err := result.validate(completionErr); err != nil {
		return err
	}
	result = cloneTerminalResult(result)

	cell.mu.Lock()
	defer cell.mu.Unlock()
	if cell.completed {
		return ErrResultAlreadyCompleted
	}
	cell.completed = true
	cell.result = result
	cell.err = completionErr
	close(cell.done)
	return nil
}

func (cell *ResultCell) Wait(ctx context.Context) (TerminalResult, error) {
	select {
	case <-cell.done:
		return cell.snapshot()
	default:
	}
	select {
	case <-cell.done:
		return cell.snapshot()
	case <-ctx.Done():
		return TerminalResult{}, ctx.Err()
	}
}

func (cell *ResultCell) Done() <-chan struct{} {
	return cell.done
}

func (cell *ResultCell) snapshot() (TerminalResult, error) {
	cell.mu.Lock()
	defer cell.mu.Unlock()
	return cloneTerminalResult(cell.result), cell.err
}

func cloneTerminalResult(result TerminalResult) TerminalResult {
	if result.Read != nil {
		read := *result.Read
		result.Read = &read
	}
	if result.Write != nil {
		write := *result.Write
		write.Repositories = append([]contract.RepositoryHandoff(nil), write.Repositories...)
		write.Verification = append([]string(nil), write.Verification...)
		result.Write = &write
	}
	return result
}
