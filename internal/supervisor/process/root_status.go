package process

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// RootStatusFD is the fixed descriptor inherited by a manager-started root for
// reporting exactly one bounded startup outcome.
const RootStatusFD = 4

const (
	RootStartupReady   = "ready"
	RootStartupFailure = "failure"
)

// RootStartupStatus is the code-only startup outcome sent by a root. It never
// carries root-controlled free-form text.
type RootStartupStatus struct {
	Status string             `json:"status"`
	Code   contract.ErrorCode `json:"code,omitempty"`
}

func (status RootStartupStatus) validate() error {
	switch status.Status {
	case RootStartupReady:
		if status.Code != "" {
			return fmt.Errorf("ready root startup status forbids a code")
		}
	case RootStartupFailure:
		if !knownRootStartupErrorCode(status.Code) {
			return fmt.Errorf("failure root startup status requires a known code")
		}
	default:
		return fmt.Errorf("unknown root startup status %q", status.Status)
	}
	return nil
}

func knownRootStartupErrorCode(code contract.ErrorCode) bool {
	switch code {
	case contract.ErrorInvalidRequest,
		contract.ErrorUnknownWorkerType,
		contract.ErrorForbiddenRPC,
		contract.ErrorProxyUnavailable,
		contract.ErrorWorkspaceRepositoryUnavailable,
		contract.ErrorProvisioningFailed,
		contract.ErrorChildFailed,
		contract.ErrorChildAborted,
		contract.ErrorHandoffRefMissing,
		contract.ErrorHandoffRefMismatch,
		contract.ErrorSessionStopping,
		contract.ErrorNotFound,
		contract.ErrorChildUnavailable,
		contract.ErrorConflict,
		contract.ErrorSaturated,
		contract.ErrorInternal:
		return true
	default:
		return false
	}
}

// EncodeRootStartupStatus validates and writes exactly one bounded JSON value.
func EncodeRootStartupStatus(writer io.Writer, status RootStartupStatus) error {
	if err := status.validate(); err != nil {
		return fmt.Errorf("validate root startup status: %w", err)
	}
	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("encode root startup status: %w", err)
	}
	if len(data) > MaxRecordBytes {
		return ErrRecordTooLarge
	}
	n, err := writer.Write(data)
	if err != nil {
		return fmt.Errorf("write root startup status: %w", err)
	}
	if n != len(data) {
		return fmt.Errorf("write root startup status: %w", io.ErrShortWrite)
	}
	return nil
}

// DecodeRootStartupStatus strictly decodes and validates exactly one bounded
// JSON value.
func DecodeRootStartupStatus(reader io.Reader) (RootStartupStatus, error) {
	var status RootStartupStatus
	if err := strictDecode(reader, &status); err != nil {
		return RootStartupStatus{}, fmt.Errorf("decode root startup status: %w", err)
	}
	if err := status.validate(); err != nil {
		return RootStartupStatus{}, fmt.Errorf("validate root startup status: %w", err)
	}
	return status, nil
}
