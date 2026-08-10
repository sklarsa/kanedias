package attachments

import (
	"errors"
	"testing"
)

func imageBytes(size int) []byte {
	data := make([]byte, size)
	copy(data, []byte("\x89PNG\r\n\x1a\n"))
	return data
}

func mustImage(t *testing.T, size int) Image {
	t.Helper()
	image, err := NewImage(imageBytes(size))
	if err != nil {
		t.Fatalf("NewImage() error: %v", err)
	}
	return image
}

func TestNewImageDetectsSupportedSignatures(t *testing.T) {
	tests := []struct {
		name, want string
		data       []byte
	}{
		{"png", "image/png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)},
		{"jpeg", "image/jpeg", []byte("\xff\xd8\xff\xe0JFIF\x00")},
		{"gif", "image/gif", []byte("GIF89a\x01\x00\x01\x00")},
		{"webp", "image/webp", []byte("RIFF\x04\x00\x00\x00WEBPVP8 ")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NewImage(test.data)
			if err != nil {
				t.Fatal(err)
			}
			if got.MIMEType != test.want {
				t.Fatalf("MIMEType = %q, want %q", got.MIMEType, test.want)
			}
		})
	}
}

func TestNewImageRejectsEmptyAndUnsupportedData(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrEmptyImage},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), ErrUnsupportedFormat},
		{"text", []byte("not an image"), ErrUnsupportedFormat},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewImage(test.data)
			if !errors.Is(err, test.want) {
				t.Fatalf("NewImage() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestNewImageCopiesCallerData(t *testing.T) {
	data := imageBytes(32)
	image, err := NewImage(data)
	if err != nil {
		t.Fatal(err)
	}
	before := image.Data[0]
	data[0] = 0
	if image.Data[0] != before {
		t.Fatal("NewImage retained caller-owned data")
	}
}

func TestNewImagePerImageSizeBoundary(t *testing.T) {
	const boundary = 3 << 20
	if _, err := NewImage(imageBytes(boundary)); err != nil {
		t.Fatalf("NewImage(%d bytes) error: %v", boundary, err)
	}
	if _, err := NewImage(imageBytes(boundary + 1)); !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("NewImage(%d bytes) error = %v, want %v", boundary+1, err, ErrImageTooLarge)
	}
}

func TestValidateExactLimits(t *testing.T) {
	if MaxImages != 4 || MaxImageBytes != 3<<20 || MaxTotalBytes != 8<<20 || MaxMessageBytes != 64<<10 {
		t.Fatalf("limits = (%d, %d, %d, %d)", MaxImages, MaxImageBytes, MaxTotalBytes, MaxMessageBytes)
	}
}

func TestValidateImageCountBoundary(t *testing.T) {
	image := mustImage(t, 32)
	four := []Image{image, image, image, image}
	if err := Validate(four); err != nil {
		t.Fatalf("Validate(4 images) error: %v", err)
	}
	five := append(append([]Image(nil), four...), image)
	if err := Validate(five); !errors.Is(err, ErrTooManyImages) {
		t.Fatalf("Validate(5 images) error = %v, want %v", err, ErrTooManyImages)
	}
}

func TestValidateAggregateSizeBoundary(t *testing.T) {
	const (
		perImage = 3 << 20
		total    = 8 << 20
	)
	exact := []Image{
		mustImage(t, perImage),
		mustImage(t, perImage),
		mustImage(t, total-2*perImage),
	}
	if err := Validate(exact); err != nil {
		t.Fatalf("Validate(%d total bytes) error: %v", total, err)
	}

	over := append([]Image(nil), exact...)
	over[2] = mustImage(t, total-2*perImage+1)
	if err := Validate(over); !errors.Is(err, ErrImagesTooLarge) {
		t.Fatalf("Validate(%d total bytes) error = %v, want %v", total+1, err, ErrImagesTooLarge)
	}
}

func TestValidateRejectsEmptyUnsupportedAndMismatchedImages(t *testing.T) {
	tests := []struct {
		name  string
		image Image
		want  error
	}{
		{"empty", Image{MIMEType: "image/png"}, ErrEmptyImage},
		{"unsupported", Image{MIMEType: "image/png", Data: []byte("plain text")}, ErrUnsupportedFormat},
		{"mismatch", Image{MIMEType: "image/jpeg", Data: imageBytes(32)}, ErrUnsupportedFormat},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := Validate([]Image{test.image}); !errors.Is(err, test.want) {
				t.Fatalf("Validate() error = %v, want %v", err, test.want)
			}
		})
	}
}
