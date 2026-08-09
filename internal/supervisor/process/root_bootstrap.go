package process

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/sklarsa/kanedias/internal/config"
)

// RootBootstrapFD is the descriptor assigned to the manager's sole inherited
// root-bootstrap endpoint.
const RootBootstrapFD = 3

// RootBootstrap is the immutable model policy transferred by the manager to a
// newly started root supervisor through its private inherited descriptor.
type RootBootstrap struct {
	Policy config.SessionModelPolicy `json:"policy"`
}

// EncodeRootBootstrap validates and clones the policy before encoding exactly
// one bounded JSON value.
func EncodeRootBootstrap(writer io.Writer, bootstrap RootBootstrap) error {
	bootstrap.Policy = bootstrap.Policy.Clone()
	if err := bootstrap.Policy.Validate(); err != nil {
		return fmt.Errorf("validate root bootstrap policy: %w", err)
	}
	data, err := json.Marshal(bootstrap)
	if err != nil {
		return fmt.Errorf("encode root bootstrap: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return ErrRecordTooLarge
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("write root bootstrap: %w", err)
	}
	return nil
}

// DecodeRootBootstrap strictly decodes one bounded JSON value and returns an
// independently owned, structurally valid policy.
func DecodeRootBootstrap(reader io.Reader) (RootBootstrap, error) {
	var bootstrap RootBootstrap
	if err := strictDecode(reader, &bootstrap); err != nil {
		return RootBootstrap{}, fmt.Errorf("decode root bootstrap: %w", err)
	}
	bootstrap.Policy = bootstrap.Policy.Clone()
	if err := bootstrap.Policy.Validate(); err != nil {
		return RootBootstrap{}, fmt.Errorf("validate root bootstrap policy: %w", err)
	}
	return bootstrap, nil
}
