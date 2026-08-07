//go:build incus

package supervisor_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func TestExpectedLiveResourceAssertionIsRootAware(t *testing.T) {
	tests := []struct {
		name         string
		node         supervisor.NodeSnapshot
		instance     string
		wantInstance string
		wantVolume   string
		wantMetadata map[string]string
	}{
		{
			name:         "root uses random provisioned instance name",
			node:         supervisor.NodeSnapshot{SessionID: "root-id", RootSessionID: "root-id", Kind: contract.ChildKindRoot, Context: contract.ContextRoot},
			instance:     "kanedias-8f4c-random",
			wantInstance: "kanedias-8f4c-random",
			wantVolume:   "kanedias-workspace-kanedias-8f4c-random",
			wantMetadata: map[string]string{
				metadataSession: "root-id", metadataParent: "", metadataRoot: "root-id",
				metadataKind: "root", metadataContext: "root", metadataWorker: "",
				metadataVolume: "kanedias-workspace-kanedias-8f4c-random", metadataRun: "run-1",
			},
		},
		{
			name:         "child uses deterministic names",
			node:         supervisor.NodeSnapshot{SessionID: "child-id", ParentSessionID: "root-id", RootSessionID: "root-id", Kind: contract.ChildKindWrite, Context: contract.ContextFork, WorkerType: "worker"},
			instance:     "session-child-id",
			wantInstance: "session-child-id",
			wantVolume:   "workspace-child-id",
			wantMetadata: map[string]string{
				metadataSession: "child-id", metadataParent: "root-id", metadataRoot: "root-id",
				metadataKind: "write", metadataContext: "fork", metadataWorker: "worker",
				metadataVolume: "workspace-child-id", metadataRun: "run-1",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance, volume, metadata, err := expectedLiveResourceAssertion(test.node, test.instance, "run-1")
			if err != nil {
				t.Fatal(err)
			}
			if instance != test.wantInstance || volume != test.wantVolume || !reflect.DeepEqual(metadata, test.wantMetadata) {
				t.Fatalf("assertion = instance %q volume %q metadata %#v", instance, volume, metadata)
			}
		})
	}
}

func TestExpectedLiveResourceAssertionRejectsWrongChildAndRootShapes(t *testing.T) {
	root := supervisor.NodeSnapshot{SessionID: "root-id", RootSessionID: "root-id", Kind: contract.ChildKindRoot, Context: contract.ContextRoot}
	if _, _, _, err := expectedLiveResourceAssertion(root, "session-root-id", "run-1"); err == nil {
		t.Fatal("root accepted child deterministic instance name")
	}
	child := supervisor.NodeSnapshot{SessionID: "child-id", ParentSessionID: "root-id", RootSessionID: "root-id", Kind: contract.ChildKindRead, Context: contract.ContextFresh, WorkerType: "reviewer"}
	if _, _, _, err := expectedLiveResourceAssertion(child, "random-child", "run-1"); err == nil {
		t.Fatal("child accepted random instance name")
	}
}

func TestCheckoutOriginPreflightCanonicalizesAndResolvesBeforePrompt(t *testing.T) {
	const head = "0123456789012345678901234567890123456789"
	resolve := func(_ context.Context, remote string) (string, error) {
		switch remote {
		case "git@github.com:owner/disposable.git", "https://github.com/owner/disposable.git", "ssh://git@github.com/owner/disposable.git":
			return head, nil
		default:
			return "", errors.New("unexpected remote")
		}
	}
	for _, origin := range []string{"git@github.com:owner/disposable.git", "ssh://git@github.com/owner/disposable.git"} {
		if err := preflightCheckoutOrigin(context.Background(), "owner/disposable", "https://github.com/owner/disposable.git", origin, resolve); err != nil {
			t.Fatalf("canonical checkout origin %q: %v", origin, err)
		}
	}

	for _, origin := range []string{"https://github.com/owner/wrong.git", "/tmp/local.git", "file:///tmp/local.git"} {
		called := false
		err := preflightCheckoutOrigin(context.Background(), "owner/disposable", "https://github.com/owner/disposable.git", origin, func(context.Context, string) (string, error) {
			called = true
			return head, nil
		})
		if err == nil {
			t.Fatalf("origin %q accepted", origin)
		}
		if called {
			t.Fatalf("origin %q was resolved before canonical rejection", origin)
		}
	}
}

func TestCheckoutOriginPreflightRejectsCredentialsAndNoncanonicalURLComponentsBeforeGit(t *testing.T) {
	unsafe := []string{
		"https://token@github.com/owner/disposable.git",
		"https://user:secret@github.com/owner/disposable.git",
		"https://github.com:443/owner/disposable.git",
		"https://github.com/owner/disposable.git?ref=main",
		"https://github.com/owner/disposable.git#main",
		"https://github.com/owner/disposable.git/extra",
		"ssh://github.com/owner/disposable.git",
		"ssh://root@github.com/owner/disposable.git",
		"ssh://git:secret@github.com/owner/disposable.git",
		"ssh://git@github.com:22/owner/disposable.git",
		"ssh://git@github.com/owner/disposable.git?ref=main",
		"ssh://git@github.com/owner/disposable.git#main",
	}
	for _, origin := range unsafe {
		t.Run(origin, func(t *testing.T) {
			called := false
			err := preflightCheckoutOrigin(context.Background(), "owner/disposable", "https://github.com/owner/disposable.git", origin, func(context.Context, string) (string, error) {
				called = true
				return strings.Repeat("a", 40), nil
			})
			if err == nil {
				t.Fatalf("unsafe origin %q accepted", origin)
			}
			if called {
				t.Fatalf("unsafe origin %q reached host git resolver", origin)
			}
		})
	}
}
