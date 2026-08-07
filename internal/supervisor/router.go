package supervisor

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

type Router struct{ node *Node }

func NewRouter(node *Node) *Router { return &Router{node: node} }

func (router *Router) Snapshot(ctx context.Context) (NodeSnapshot, error) {
	root := router.node.Snapshot()
	entries := router.node.children.snapshot()
	children := make([]NodeSnapshot, len(entries))
	errs := make([]error, len(entries))
	var wait sync.WaitGroup
	for index, entry := range entries {
		client, _, _ := entry.values()
		if client == nil {
			children[index] = entry.fallback
			continue
		}
		wait.Add(1)
		go func(index int, entry *childEntry, client DescendantClient) {
			defer wait.Done()
			children[index], errs[index] = client.Snapshot(ctx)
			if errs[index] != nil {
				errs[index] = childUnavailable(entry.id, errs[index])
			}
		}(index, entry, client)
	}
	wait.Wait()
	for _, err := range errs {
		if err != nil {
			return NodeSnapshot{}, err
		}
	}
	sortSnapshots(children)
	root.Children = children
	return root, nil
}

func sortSnapshots(children []NodeSnapshot) {
	for index := range children {
		sortSnapshots(children[index].Children)
	}
	sort.Slice(children, func(i, j int) bool { return children[i].SessionID < children[j].SessionID })
}

func containsSession(snapshot NodeSnapshot, target string) bool {
	if snapshot.SessionID == target {
		return true
	}
	for _, child := range snapshot.Children {
		if containsSession(child, target) {
			return true
		}
	}
	return false
}

func (router *Router) descendantFor(ctx context.Context, target string) (DescendantClient, error) {
	if direct := router.node.children.get(target); direct != nil {
		client, _, _ := direct.values()
		if client == nil {
			return nil, childUnavailable(direct.id, nil)
		}
		return client, nil
	}
	// Only immutable entry pointers are copied while locked. All routed HTTP is
	// performed after the registry lock has been released.
	var unavailable error
	for _, entry := range router.node.children.snapshot() {
		client, _, _ := entry.values()
		if client == nil {
			continue
		}
		snapshot, err := client.Snapshot(ctx)
		if err != nil {
			if unavailable == nil {
				unavailable = childUnavailable(entry.id, err)
			}
			continue
		}
		if containsSession(snapshot, target) {
			return client, nil
		}
	}
	if unavailable != nil {
		return nil, unavailable
	}
	return nil, contract.NewError(contract.ErrorNotFound, "session not found")
}

func (router *Router) Workers(context.Context) []contract.WorkerSummary {
	return router.node.WorkerSummaries()
}

func (router *Router) CallRPC(ctx context.Context, target string, command json.RawMessage) (json.RawMessage, error) {
	if target == router.node.identity.Snapshot().SessionID {
		return router.node.CallRPC(ctx, command)
	}
	client, err := router.descendantFor(ctx, target)
	if err != nil {
		return nil, err
	}
	response, err := client.CallRPC(ctx, target, command)
	if err != nil {
		return nil, childUnavailable(target, err)
	}
	return response, nil
}

func (router *Router) AnswerQuestion(ctx context.Context, target, question string, answer json.RawMessage) error {
	if target == router.node.identity.Snapshot().SessionID {
		return router.node.AnswerQuestion(ctx, question, answer)
	}
	client, err := router.descendantFor(ctx, target)
	if err != nil {
		return err
	}
	if err := client.AnswerQuestion(ctx, target, question, answer); err != nil {
		return childUnavailable(target, err)
	}
	return nil
}

func (router *Router) Subscribe(context.Context) (Subscription, error) {
	return router.node.broker.Subscribe(), nil
}

func (router *Router) Stop(ctx context.Context, target string) error {
	if target == router.node.identity.Snapshot().SessionID {
		return router.node.Stop(ctx, StopReasonRequested)
	}
	client, err := router.descendantFor(ctx, target)
	if err != nil {
		return err
	}
	if err := client.Stop(ctx, target); err != nil {
		return childUnavailable(target, err)
	}
	return nil
}

func (router *Router) CreateChild(ctx context.Context, parent string, request contract.CreateChildRequest) (TerminalResult, error) {
	return router.node.CreateChild(ctx, parent, request)
}
