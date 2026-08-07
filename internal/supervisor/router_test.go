package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

type fakeDescendantClient struct {
	mu       sync.Mutex
	snapshot NodeSnapshot
	snapErr  error
	rpcCalls []string
	answers  []string
	stops    []string
	events   Subscription
	entered  chan struct{}
	release  chan struct{}
}

func (client *fakeDescendantClient) Snapshot(context.Context) (NodeSnapshot, error) {
	if client.entered != nil {
		select {
		case client.entered <- struct{}{}:
		default:
		}
	}
	if client.release != nil {
		<-client.release
	}
	return client.snapshot, client.snapErr
}
func (client *fakeDescendantClient) Subscribe(context.Context) (Subscription, error) {
	return client.events, nil
}
func (client *fakeDescendantClient) CallRPC(_ context.Context, target string, _ json.RawMessage) (json.RawMessage, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.rpcCalls = append(client.rpcCalls, target)
	return json.RawMessage(`{"target":"` + target + `"}`), nil
}
func (client *fakeDescendantClient) AnswerQuestion(_ context.Context, target, question string, _ json.RawMessage) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.answers = append(client.answers, target+":"+question)
	return nil
}
func (client *fakeDescendantClient) Stop(_ context.Context, target string) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.stops = append(client.stops, target)
	return nil
}

func routingNode(t *testing.T, clients map[string]*fakeDescendantClient) *Node {
	t.Helper()
	node := &Node{identity: testRootIdentity(t), broker: NewEventBroker(), state: LifecycleReady, done: make(chan struct{}), startupDone: make(chan struct{})}
	node.children = newChildRegistry()
	for id, client := range clients {
		node.children.add(&childEntry{id: id, client: client})
	}
	return node
}

func TestRouterBuildsDeterministicThreeLevelTreeAndRoutesExactTargets(t *testing.T) {
	grandchild := NodeSnapshot{SessionID: "grandchild", ParentSessionID: "child-b", RootSessionID: "root-1", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Lifecycle: "ready", Questions: []QuestionSummary{}, Children: []NodeSnapshot{}}
	childB := &fakeDescendantClient{snapshot: NodeSnapshot{SessionID: "child-b", ParentSessionID: "root-1", RootSessionID: "root-1", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Lifecycle: "ready", Questions: []QuestionSummary{}, Children: []NodeSnapshot{grandchild}}}
	childA := &fakeDescendantClient{snapshot: NodeSnapshot{SessionID: "child-a", ParentSessionID: "root-1", RootSessionID: "root-1", Kind: contract.ChildKindRead, Context: contract.ContextFresh, Lifecycle: "ready", Questions: []QuestionSummary{}, Children: []NodeSnapshot{}}}
	router := NewRouter(routingNode(t, map[string]*fakeDescendantClient{"child-b": childB, "child-a": childA}))

	snapshot, err := router.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{snapshot.Children[0].SessionID, snapshot.Children[1].SessionID}; !reflect.DeepEqual(got, []string{"child-a", "child-b"}) {
		t.Fatalf("children = %v", got)
	}
	if snapshot.Children[1].Children[0].SessionID != "grandchild" {
		t.Fatalf("tree = %#v", snapshot)
	}

	if _, err := router.CallRPC(context.Background(), "grandchild", json.RawMessage(`{"type":"get_state"}`)); err != nil {
		t.Fatal(err)
	}
	if err := router.AnswerQuestion(context.Background(), "grandchild", "q1", json.RawMessage(`{"confirmed":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := router.Stop(context.Background(), "grandchild"); err != nil {
		t.Fatal(err)
	}
	childB.mu.Lock()
	defer childB.mu.Unlock()
	if !reflect.DeepEqual(childB.rpcCalls, []string{"grandchild"}) || !reflect.DeepEqual(childB.answers, []string{"grandchild:q1"}) || !reflect.DeepEqual(childB.stops, []string{"grandchild"}) {
		t.Fatalf("routed calls rpc=%v answers=%v stops=%v", childB.rpcCalls, childB.answers, childB.stops)
	}
}

func TestRouterDoesNotHoldRegistryLockDuringDescendantHTTP(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	client := &fakeDescendantClient{snapshot: NodeSnapshot{SessionID: "child", Children: []NodeSnapshot{}}, entered: entered, release: release}
	node := routingNode(t, map[string]*fakeDescendantClient{"child": client})
	done := make(chan error, 1)
	go func() { _, err := NewRouter(node).Snapshot(context.Background()); done <- err }()
	<-entered
	added := make(chan struct{})
	go func() { node.children.add(&childEntry{id: "sibling", client: &fakeDescendantClient{}}); close(added) }()
	select {
	case <-added:
	case <-time.After(time.Second):
		t.Fatal("registry lock held across descendant Snapshot")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRouterMapsDescendantSocketFailureToTypedGatewayError(t *testing.T) {
	client := &fakeDescendantClient{snapshot: NodeSnapshot{SessionID: "child"}, snapErr: errors.New("dial unix: refused")}
	_, err := NewRouter(routingNode(t, map[string]*fakeDescendantClient{"child": client})).Snapshot(context.Background())
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorChildUnavailable {
		t.Fatalf("error = %v", err)
	}
}
