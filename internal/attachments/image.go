package attachments

import (
	"errors"
	"net/http"
)

const (
	MaxImages       = 4
	MaxImageBytes   = 3 << 20
	MaxTotalBytes   = 8 << 20
	MaxMessageBytes = 64 << 10
)

var (
	ErrEmptyImage        = errors.New("image is empty")
	ErrUnsupportedFormat = errors.New("image format is unsupported")
	ErrTooManyImages     = errors.New("too many images")
	ErrImageTooLarge     = errors.New("image exceeds per-image limit")
	ErrImagesTooLarge    = errors.New("images exceed aggregate limit")
)

type Image struct {
	MIMEType string
	Data     []byte
}

// NewImage detects the image format from its content and takes ownership of a
// copy of data.
func NewImage(data []byte) (Image, error) {
	mimeType, err := detectMIMEType(data)
	if err != nil {
		return Image{}, err
	}
	image := Image{
		MIMEType: mimeType,
		Data:     append([]byte(nil), data...),
	}
	if err := Validate([]Image{image}); err != nil {
		return Image{}, err
	}
	return image, nil
}

// Validate enforces image count and size limits and verifies that every
// declared MIME type matches the image content.
func Validate(images []Image) error {
	if len(images) > MaxImages {
		return ErrTooManyImages
	}

	totalBytes := 0
	for _, image := range images {
		if len(image.Data) == 0 {
			return ErrEmptyImage
		}
		if len(image.Data) > MaxImageBytes {
			return ErrImageTooLarge
		}
		totalBytes += len(image.Data)
		if totalBytes > MaxTotalBytes {
			return ErrImagesTooLarge
		}

		detected, err := detectMIMEType(image.Data)
		if err != nil {
			return err
		}
		if detected != normalizeMIMEType(image.MIMEType) {
			return ErrUnsupportedFormat
		}
	}
	return nil
}

func detectMIMEType(data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrEmptyImage
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp", nil
	}

	mimeType := normalizeMIMEType(http.DetectContentType(data))
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mimeType, nil
	default:
		return "", ErrUnsupportedFormat
	}
}

func normalizeMIMEType(mimeType string) string {
	if mimeType == "image/jpg" {
		return "image/jpeg"
	}
	return mimeType
}
