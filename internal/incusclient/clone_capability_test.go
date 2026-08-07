package incusclient

import (
	"strings"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
)

func TestValidateCOWPoolAcceptsOnlyCreatedBtrfs(t *testing.T) {
	if err := ValidateCOWPool(&api.StoragePool{Name: "default", Driver: "btrfs", Status: api.StoragePoolStatusCreated}); err != nil {
		t.Fatalf("ValidateCOWPool(created btrfs) error = %v", err)
	}

	for _, driver := range []string{"dir", "zfs", "lvm", "unknown", ""} {
		t.Run("reject_"+driver, func(t *testing.T) {
			err := ValidateCOWPool(&api.StoragePool{Name: "default", Driver: driver, Status: api.StoragePoolStatusCreated})
			if err == nil || !strings.Contains(err.Error(), "unsupported non-attested driver") {
				t.Fatalf("ValidateCOWPool(%q) error = %v, want unsupported driver", driver, err)
			}
		})
	}
}

func TestValidateCOWPoolRejectsPoolThatIsNotReady(t *testing.T) {
	for _, status := range []string{api.StoragePoolStatusPending, api.StoragePoolStatusErrored, api.StoragePoolStatusUnknown, ""} {
		t.Run(status, func(t *testing.T) {
			err := ValidateCOWPool(&api.StoragePool{Name: "default", Driver: "btrfs", Status: status})
			if err == nil || !strings.Contains(err.Error(), "not ready") {
				t.Fatalf("ValidateCOWPool(%q) error = %v, want not-ready error", status, err)
			}
		})
	}
}

func TestEffectiveRootPoolUsesExpandedRootDevice(t *testing.T) {
	instance := &api.Instance{
		InstancePut: api.InstancePut{Devices: api.DevicesMap{"root": {"type": "disk", "pool": "local"}}},
		ExpandedDevices: api.DevicesMap{
			"root": {"type": "disk", "path": "/", "pool": "effective"},
		},
	}
	got, err := EffectiveRootPool(instance)
	if err != nil {
		t.Fatal(err)
	}
	if got != "effective" {
		t.Fatalf("EffectiveRootPool() = %q, want effective", got)
	}
}

func TestEffectiveRootPoolFailsClosedWhenExpandedRootPoolMissing(t *testing.T) {
	for _, instance := range []*api.Instance{
		nil,
		{},
		{ExpandedDevices: api.DevicesMap{"root": {"type": "disk", "path": "/"}}},
	} {
		if _, err := EffectiveRootPool(instance); err == nil {
			t.Fatalf("EffectiveRootPool(%#v) error = nil, want missing effective root pool", instance)
		}
	}
}
