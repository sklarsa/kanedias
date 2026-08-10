package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/sklarsa/kanedias/internal/attachments"
	"github.com/sklarsa/kanedias/internal/manager"
)

const (
	neutralImageMessage      = "Please inspect the attached image(s)."
	maxMessageMultipartBytes = attachments.MaxTotalBytes + attachments.MaxMessageBytes + (128 << 10)

	invalidMessageUpload = "The message upload was not valid."
	imageLimitsExceeded  = "The image attachment limits were exceeded."
	unsupportedImage     = "Only PNG, JPEG, GIF, and WebP images are supported."
)

type messageRequest struct {
	Message string
	Images  []attachments.Image
}

type messageDecodeError struct {
	Status  int
	Message string
	Err     error
}

func (e *messageDecodeError) Error() string {
	return fmt.Sprintf("decode message request: %v", e.Err)
}

func (e *messageDecodeError) Unwrap() error {
	return e.Err
}

func invalidMessageError(err error) error {
	return &messageDecodeError{Status: http.StatusBadRequest, Message: invalidMessageUpload, Err: err}
}

func messageLimitError(err error) error {
	return &messageDecodeError{Status: http.StatusRequestEntityTooLarge, Message: imageLimitsExceeded, Err: err}
}

func unsupportedImageError(err error) error {
	return &messageDecodeError{Status: http.StatusUnsupportedMediaType, Message: unsupportedImage, Err: err}
}

// decodeMessageRequest decodes a strict, bounded multipart request without
// materializing files on disk. It does not return until the complete multipart
// stream has been validated.
func decodeMessageRequest(w http.ResponseWriter, r *http.Request) (messageRequest, error) {
	if r.ContentLength > maxMessageMultipartBytes {
		return messageRequest{}, classifyMultipartReadError(&http.MaxBytesError{Limit: maxMessageMultipartBytes})
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageMultipartBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		return messageRequest{}, invalidMessageError(err)
	}

	var request messageRequest
	seenMessage := false
	totalImageBytes := 0
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			if _, drainErr := io.Copy(io.Discard, r.Body); drainErr != nil {
				return messageRequest{}, classifyMultipartReadError(drainErr)
			}
			break
		}
		if nextErr != nil {
			return messageRequest{}, classifyMultipartReadError(nextErr)
		}

		name := part.FormName()
		filename := part.FileName()
		switch name {
		case "message":
			if seenMessage || filename != "" {
				_ = part.Close()
				return messageRequest{}, invalidMessageError(errors.New("message part must be a unique form field"))
			}
			data, readErr := io.ReadAll(io.LimitReader(part, attachments.MaxMessageBytes+1))
			closeErr := part.Close()
			if readErr != nil {
				return messageRequest{}, classifyMultipartReadError(readErr)
			}
			if closeErr != nil {
				return messageRequest{}, classifyMultipartReadError(closeErr)
			}
			if len(data) > attachments.MaxMessageBytes {
				return messageRequest{}, messageLimitError(attachments.ErrImagesTooLarge)
			}
			seenMessage = true
			request.Message = string(data)

		case "image":
			if filename == "" {
				_ = part.Close()
				return messageRequest{}, invalidMessageError(errors.New("image part must be a file"))
			}
			if len(request.Images) >= attachments.MaxImages {
				_ = part.Close()
				return messageRequest{}, messageLimitError(attachments.ErrTooManyImages)
			}
			data, readErr := io.ReadAll(io.LimitReader(part, attachments.MaxImageBytes+1))
			closeErr := part.Close()
			if readErr != nil {
				return messageRequest{}, classifyMultipartReadError(readErr)
			}
			if closeErr != nil {
				return messageRequest{}, classifyMultipartReadError(closeErr)
			}
			if len(data) > attachments.MaxImageBytes {
				return messageRequest{}, messageLimitError(attachments.ErrImageTooLarge)
			}
			totalImageBytes += len(data)
			if totalImageBytes > attachments.MaxTotalBytes {
				return messageRequest{}, messageLimitError(attachments.ErrImagesTooLarge)
			}
			image, imageErr := attachments.NewImage(data)
			if imageErr != nil {
				return messageRequest{}, unsupportedImageError(imageErr)
			}
			request.Images = append(request.Images, image)

		default:
			_ = part.Close()
			return messageRequest{}, invalidMessageError(fmt.Errorf("unexpected multipart field %q", name))
		}
	}

	if !seenMessage {
		return messageRequest{}, invalidMessageError(errors.New("message field is required"))
	}
	if len(request.Images) > 0 && strings.TrimSpace(request.Message) == "" {
		request.Message = neutralImageMessage
	}
	return request, nil
}

func classifyMultipartReadError(err error) error {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return messageLimitError(err)
	}
	return invalidMessageError(err)
}

func makeMessageHandler(fleet fleetManager, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request, err := decodeMessageRequest(w, r)
		if err != nil {
			var decodeErr *messageDecodeError
			if !errors.As(err, &decodeErr) {
				decodeErr = &messageDecodeError{Status: http.StatusBadRequest, Message: invalidMessageUpload, Err: err}
			}
			writeMessageJSON(w, decodeErr.Status, false, decodeErr.Message)
			return
		}

		sessionID := chi.URLParam(r, "sessionID")
		if err := fleet.SendMessage(r.Context(), sessionID, request.Message, request.Images); err != nil {
			if errors.Is(err, manager.ErrImageInputUnsupported) {
				writeMessageJSON(w, http.StatusConflict, false, "The selected model does not support image input.")
				return
			}
			logger.Error("send message failed", "method", r.Method, "path", r.URL.Path, "sessionID", sessionID, "error", err)
			writeMessageJSON(w, http.StatusServiceUnavailable, false, "The message could not be sent.")
			return
		}
		writeMessageJSON(w, http.StatusAccepted, true, "")
	}
}

type messageResponse struct {
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

func writeMessageJSON(w http.ResponseWriter, status int, accepted bool, message string) {
	encoded, err := json.Marshal(messageResponse{Accepted: accepted, Error: message})
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}
