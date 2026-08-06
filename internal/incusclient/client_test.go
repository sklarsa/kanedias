package incusclient

import (
	"context"
	"testing"
)

type poolServer struct {
	names []string
	err   error
	calls int
}

func (s *poolServer) GetStoragePoolNames() ([]string, error) {
	s.calls++
	return s.names, s.err
}

func TestResolvePoolUsesConfiguredNameWithoutQuery(t *testing.T) {
	server := &poolServer{}
	got, err := resolvePool(server, "fast")
	if err != nil {
		t.Fatal(err)
	}
	if got != "fast" {
		t.Fatalf("resolvePool() = %q, want fast", got)
	}
	if server.calls != 0 {
		t.Fatalf("GetStoragePoolNames calls = %d, want 0", server.calls)
	}
}

func TestResolvePoolRequiresExactlyOnePoolWhenUnconfigured(t *testing.T) {
	tests := []struct {
		name    string
		names   []string
		want    string
		wantErr bool
	}{
		{name: "one", names: []string{"default"}, want: "default"},
		{name: "zero", wantErr: true},
		{name: "multiple", names: []string{"default", "fast"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePool(&poolServer{names: tt.names}, "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolvePool() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("resolvePool() = %q, want %q", got, tt.want)
			}
		})
	}
}

type fakeOperation struct {
	waitContext context.Context
	waitErr     error
}

func (o *fakeOperation) WaitContext(ctx context.Context) error {
	o.waitContext = ctx
	return o.waitErr
}

func TestWaitOperationPassesContext(t *testing.T) {
	key := struct{}{}
	ctx := context.WithValue(context.Background(), key, "value")
	op := &fakeOperation{}
	if err := waitOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if op.waitContext != ctx {
		t.Fatal("WaitContext did not receive supplied context")
	}
}
