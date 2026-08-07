package provision

import "sync"

type OwnedResource struct {
	Name      string
	Submitted bool
	Confirmed bool
}

type OwnershipSnapshot struct {
	Instance OwnedResource
	Volume   OwnedResource
}

type Ownership struct {
	mu       sync.RWMutex
	instance OwnedResource
	volume   OwnedResource
}

func (ownership *Ownership) RecordInstanceSubmitted(name string) {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	ownership.instance.Name = name
	ownership.instance.Submitted = true
}

func (ownership *Ownership) RecordInstanceConfirmed() {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ownership.instance.Submitted {
		ownership.instance.Confirmed = true
	}
}

func (ownership *Ownership) RecordVolumeSubmitted(name string) {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	ownership.volume.Name = name
	ownership.volume.Submitted = true
}

func (ownership *Ownership) RecordVolumeConfirmed() {
	ownership.mu.Lock()
	defer ownership.mu.Unlock()
	if ownership.volume.Submitted {
		ownership.volume.Confirmed = true
	}
}

func (ownership *Ownership) Snapshot() OwnershipSnapshot {
	ownership.mu.RLock()
	defer ownership.mu.RUnlock()
	return OwnershipSnapshot{
		Instance: ownership.instance,
		Volume:   ownership.volume,
	}
}
