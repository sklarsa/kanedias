package contract

import (
	"errors"
	"fmt"
	"net/http"
)

type ErrorCode string

const (
	ErrorInvalidRequest                 ErrorCode = "invalid_request"
	ErrorUnknownWorkerType              ErrorCode = "unknown_worker_type"
	ErrorForbiddenRPC                   ErrorCode = "forbidden_rpc"
	ErrorProxyUnavailable               ErrorCode = "proxy_unavailable"
	ErrorWorkspaceRepositoryUnavailable ErrorCode = "workspace_repository_unavailable"
	ErrorProvisioningFailed             ErrorCode = "provisioning_failed"
	ErrorChildFailed                    ErrorCode = "child_failed"
	ErrorChildAborted                   ErrorCode = "child_aborted"
	ErrorHandoffRefMissing              ErrorCode = "handoff_ref_missing"
	ErrorHandoffRefMismatch             ErrorCode = "handoff_ref_mismatch"
	ErrorSessionStopping                ErrorCode = "session_stopping"
	ErrorNotFound                       ErrorCode = "not_found"
	ErrorChildUnavailable               ErrorCode = "child_unavailable"
	ErrorConflict                       ErrorCode = "conflict"
	ErrorSaturated                      ErrorCode = "saturated"
	ErrorInternal                       ErrorCode = "internal"
)

type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

func (err *Error) Error() string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", err.Code, err.Message)
}

func (code ErrorCode) HTTPStatus() int {
	switch code {
	case ErrorInvalidRequest, ErrorUnknownWorkerType:
		return http.StatusBadRequest
	case ErrorForbiddenRPC, ErrorChildAborted, ErrorHandoffRefMissing,
		ErrorHandoffRefMismatch, ErrorSessionStopping, ErrorConflict:
		return http.StatusConflict
	case ErrorNotFound:
		return http.StatusNotFound
	case ErrorSaturated:
		return http.StatusTooManyRequests
	case ErrorWorkspaceRepositoryUnavailable:
		return http.StatusServiceUnavailable
	case ErrorProxyUnavailable, ErrorChildFailed, ErrorChildUnavailable:
		return http.StatusBadGateway
	case ErrorProvisioningFailed, ErrorInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func HTTPStatus(err error) int {
	var contractErr *Error
	if errors.As(err, &contractErr) {
		return contractErr.Code.HTTPStatus()
	}
	return http.StatusInternalServerError
}
