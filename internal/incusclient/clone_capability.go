package incusclient

import (
	"fmt"

	"github.com/lxc/incus/v7/shared/api"
)

// ValidateCOWPool enforces the storage policy whose clone behavior has been
// live-attested for both instance roots and custom volumes.
func ValidateCOWPool(pool *api.StoragePool) error {
	if pool == nil {
		return fmt.Errorf("storage pool is missing")
	}
	if pool.Status != api.StoragePoolStatusCreated {
		return fmt.Errorf("storage pool %q is not ready: %s", pool.Name, pool.Status)
	}
	if pool.Driver != "btrfs" {
		return fmt.Errorf("storage pool %q uses unsupported non-attested driver %q", pool.Name, pool.Driver)
	}
	return nil
}

// EffectiveRootPool returns the pool selected after Incus expands profiles and
// local devices for an instance.
func EffectiveRootPool(instance *api.Instance) (string, error) {
	if instance == nil {
		return "", fmt.Errorf("parent Incus instance is missing")
	}
	root, ok := instance.ExpandedDevices["root"]
	if !ok {
		return "", fmt.Errorf("parent Incus instance %q has no effective root device", instance.Name)
	}
	pool := root["pool"]
	if pool == "" {
		return "", fmt.Errorf("parent Incus instance %q effective root device has no storage pool", instance.Name)
	}
	return pool, nil
}
