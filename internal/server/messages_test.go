package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/attachments"
	"github.com/sklarsa/kanedias/internal/manager"
)

type multipartFixture struct {
	name string
	data []byte
}

func multipartMessage(t *testing.T, message string, images ...multipartFixture) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("message", message); err != nil {
		t.Fatal(err)
	}
	for _, image := range images {
		part, err := writer.CreateFormFile("image", image.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(image.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return writer.FormDataContentType(), body.Bytes()
}

func multipartWithParts(t *testing.T, parts func(*multipart.Writer)) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	parts(writer)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return writer.FormDataContentType(), body.Bytes()
}

func signatureImage(signature []byte, size int) []byte {
	if size < len(signature) {
		size = len(signature)
	}
	data := make([]byte, size)
	copy(data, signature)
	return data
}

func pngImage(size int) []byte {
	return signatureImage([]byte("\x89PNG\r\n\x1a\n"), size)
}

func decodeMultipartForTest(t *testing.T, contentType string, body []byte) (messageRequest, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/ui/sessions/s/messages", bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	return decodeMessageRequest(httptest.NewRecorder(), r)
}

func TestMultipartMessageDecodesValidSignatures(t *testing.T) {
	fixtures := []struct {
		name     string
		data     []byte
		wantMIME string
	}{
		{name: "png", data: pngImage(32), wantMIME: "image/png"},
		{name: "jpeg", data: signatureImage([]byte("\xff\xd8\xff\xdb"), 32), wantMIME: "image/jpeg"},
		{name: "gif", data: signatureImage([]byte("GIF89a"), 32), wantMIME: "image/gif"},
		{name: "webp", data: signatureImage([]byte("RIFF\x18\x00\x00\x00WEBPVP8 "), 32), wantMIME: "image/webp"},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			contentType, body := multipartMessage(t, "inspect", multipartFixture{name: fixture.name + ".bin", data: fixture.data})
			got, err := decodeMultipartForTest(t, contentType, body)
			if err != nil {
				t.Fatalf("decodeMessageRequest: %v", err)
			}
			if got.Message != "inspect" || len(got.Images) != 1 || got.Images[0].MIMEType != fixture.wantMIME || !bytes.Equal(got.Images[0].Data, fixture.data) {
				t.Fatalf("decoded request = %#v", got)
			}
		})
	}
}

func TestMultipartMessageAcceptsZeroAndFourImages(t *testing.T) {
	for _, count := range []int{0, attachments.MaxImages} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			images := make([]multipartFixture, count)
			for i := range images {
				images[i] = multipartFixture{name: "image.png", data: pngImage(16)}
			}
			contentType, body := multipartMessage(t, "hello", images...)
			got, err := decodeMultipartForTest(t, contentType, body)
			if err != nil {
				t.Fatalf("decodeMessageRequest: %v", err)
			}
			if len(got.Images) != count {
				t.Fatalf("image count = %d, want %d", len(got.Images), count)
			}
		})
	}
}

func TestMultipartMessageAppliesNeutralImagePrompt(t *testing.T) {
	contentType, body := multipartMessage(t, " \n\t ", multipartFixture{name: "image.png", data: pngImage(16)})
	got, err := decodeMultipartForTest(t, contentType, body)
	if err != nil {
		t.Fatalf("decodeMessageRequest: %v", err)
	}
	if got.Message != neutralImageMessage {
		t.Fatalf("message = %q, want %q", got.Message, neutralImageMessage)
	}
}

func TestMultipartMessageRejectsLimits(t *testing.T) {
	tests := []struct {
		name    string
		message string
		images  []multipartFixture
	}{
		{name: "five images", message: "inspect", images: []multipartFixture{{"1.png", pngImage(8)}, {"2.png", pngImage(8)}, {"3.png", pngImage(8)}, {"4.png", pngImage(8)}, {"5.png", pngImage(8)}}},
		{name: "image over three MiB", message: "inspect", images: []multipartFixture{{"large.png", pngImage(attachments.MaxImageBytes + 1)}}},
		{name: "aggregate over eight MiB", message: "inspect", images: []multipartFixture{{"1.png", pngImage(attachments.MaxImageBytes)}, {"2.png", pngImage(attachments.MaxImageBytes)}, {"3.png", pngImage(attachments.MaxTotalBytes - 2*attachments.MaxImageBytes + 1)}}},
		{name: "message over 64 KiB", message: strings.Repeat("m", attachments.MaxMessageBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contentType, body := multipartMessage(t, test.message, test.images...)
			_, err := decodeMultipartForTest(t, contentType, body)
			var decodeErr *messageDecodeError
			if !errors.As(err, &decodeErr) || decodeErr.Status != http.StatusRequestEntityTooLarge {
				t.Fatalf("error = %v, want 413 messageDecodeError", err)
			}
		})
	}
}

func TestMultipartMessageRejectsInvalidStructure(t *testing.T) {
	tests := []struct {
		name        string
		contentType func(*testing.T) (string, []byte)
	}{
		{name: "missing message", contentType: func(t *testing.T) (string, []byte) {
			return multipartWithParts(t, func(w *multipart.Writer) {
				part, err := w.CreateFormFile("image", "image.png")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = part.Write(pngImage(8))
			})
		}},
		{name: "duplicate message", contentType: func(t *testing.T) (string, []byte) {
			return multipartWithParts(t, func(w *multipart.Writer) {
				if err := w.WriteField("message", "one"); err != nil {
					t.Fatal(err)
				}
				if err := w.WriteField("message", "two"); err != nil {
					t.Fatal(err)
				}
			})
		}},
		{name: "unknown field", contentType: func(t *testing.T) (string, []byte) {
			return multipartWithParts(t, func(w *multipart.Writer) {
				if err := w.WriteField("message", "hello"); err != nil {
					t.Fatal(err)
				}
				if err := w.WriteField("other", "value"); err != nil {
					t.Fatal(err)
				}
			})
		}},
		{name: "non-file image", contentType: func(t *testing.T) (string, []byte) {
			return multipartWithParts(t, func(w *multipart.Writer) {
				if err := w.WriteField("message", "hello"); err != nil {
					t.Fatal(err)
				}
				if err := w.WriteField("image", string(pngImage(8))); err != nil {
					t.Fatal(err)
				}
			})
		}},
		{name: "file message", contentType: func(t *testing.T) (string, []byte) {
			return multipartWithParts(t, func(w *multipart.Writer) {
				part, err := w.CreateFormFile("message", "message.txt")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.WriteString(part, "hello")
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contentType, body := test.contentType(t)
			_, err := decodeMultipartForTest(t, contentType, body)
			var decodeErr *messageDecodeError
			if !errors.As(err, &decodeErr) || decodeErr.Status != http.StatusBadRequest {
				t.Fatalf("error = %v, want 400 messageDecodeError", err)
			}
		})
	}
}

func TestMultipartMessageRejectsMalformedAndTruncatedBodies(t *testing.T) {
	contentType, body := multipartMessage(t, "hello", multipartFixture{name: "image.png", data: pngImage(16)})
	tests := []struct {
		name        string
		contentType string
		body        []byte
	}{
		{name: "malformed boundary", contentType: "multipart/form-data; boundary=does-not-occur", body: body},
		{name: "missing boundary", contentType: "multipart/form-data", body: body},
		{name: "truncated", contentType: contentType, body: body[:len(body)-8]},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeMultipartForTest(t, test.contentType, test.body)
			var decodeErr *messageDecodeError
			if !errors.As(err, &decodeErr) || decodeErr.Status != http.StatusBadRequest {
				t.Fatalf("error = %v, want 400 messageDecodeError", err)
			}
		})
	}
}

func TestMultipartMessageRejectsUnsupportedImageContent(t *testing.T) {
	for _, fixture := range []multipartFixture{
		{name: "drawing.png", data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
		{name: "notes.png", data: []byte("plain text disguised as png")},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			contentType, body := multipartMessage(t, "inspect", fixture)
			_, err := decodeMultipartForTest(t, contentType, body)
			var decodeErr *messageDecodeError
			if !errors.As(err, &decodeErr) || decodeErr.Status != http.StatusUnsupportedMediaType {
				t.Fatalf("error = %v, want 415 messageDecodeError", err)
			}
		})
	}
}

func serveMultipartMessage(t *testing.T, handler http.Handler, cookie *http.Cookie, sessionID, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/ui/sessions/"+sessionID+"/messages", bytes.NewReader(body))
	r.Host = effectiveAddrForTests
	r.Header.Set("Content-Type", contentType)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestMessageEndpointExactBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		message    string
		images     []multipartFixture
		wantStatus int
		wantBody   string
	}{
		{name: "directive exact", message: strings.Repeat("m", attachments.MaxMessageBytes), wantStatus: http.StatusAccepted, wantBody: `{"accepted":true}`},
		{name: "directive over", message: strings.Repeat("m", attachments.MaxMessageBytes+1), wantStatus: http.StatusRequestEntityTooLarge, wantBody: `{"accepted":false,"error":"The directive must be 64 KiB or smaller."}`},
		{name: "image exact", message: "inspect", images: []multipartFixture{{"exact.png", pngImage(attachments.MaxImageBytes)}}, wantStatus: http.StatusAccepted, wantBody: `{"accepted":true}`},
		{name: "image over", message: "inspect", images: []multipartFixture{{"over.png", pngImage(attachments.MaxImageBytes + 1)}}, wantStatus: http.StatusRequestEntityTooLarge, wantBody: `{"accepted":false,"error":"The image attachment limits were exceeded."}`},
		{name: "aggregate exact", message: "inspect", images: []multipartFixture{{"one.png", pngImage(attachments.MaxImageBytes)}, {"two.png", pngImage(attachments.MaxImageBytes)}, {"three.png", pngImage(attachments.MaxTotalBytes - 2*attachments.MaxImageBytes)}}, wantStatus: http.StatusAccepted, wantBody: `{"accepted":true}`},
		{name: "aggregate over", message: "inspect", images: []multipartFixture{{"one.png", pngImage(attachments.MaxImageBytes)}, {"two.png", pngImage(attachments.MaxImageBytes)}, {"three.png", pngImage(attachments.MaxTotalBytes - 2*attachments.MaxImageBytes + 1)}}, wantStatus: http.StatusRequestEntityTooLarge, wantBody: `{"accepted":false,"error":"The image attachment limits were exceeded."}`},
		{name: "empty image", message: "inspect", images: []multipartFixture{{"empty.png", nil}}, wantStatus: http.StatusUnsupportedMediaType, wantBody: `{"accepted":false,"error":"Only PNG, JPEG, GIF, and WebP images are supported."}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fleet := newStreamFakeFleet()
			handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
			contentType, body := multipartMessage(t, test.message, test.images...)
			response := serveMultipartMessage(t, handler, cookie, "session-1", contentType, body)
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody {
				t.Fatalf("response = %d %q, want %d %q", response.Code, response.Body.String(), test.wantStatus, test.wantBody)
			}
			if test.wantStatus == http.StatusAccepted && fleet.sentMessage.sessionID != "session-1" {
				t.Fatalf("accepted request did not dispatch: %#v", fleet.sentMessage)
			}
			if test.wantStatus != http.StatusAccepted && fleet.sentMessage.sessionID != "" {
				t.Fatalf("rejected request dispatched: %#v", fleet.sentMessage)
			}
		})
	}
}

func TestMessageEndpointAcceptsValidUpload(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
	contentType, body := multipartMessage(t, "inspect", multipartFixture{name: "screen.png", data: pngImage(16)})
	response := serveMultipartMessage(t, handler, cookie, "session-1", contentType, body)
	if response.Code != http.StatusAccepted || response.Body.String() != `{"accepted":true}` {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	if fleet.sentMessage.sessionID != "session-1" || fleet.sentMessage.message != "inspect" || len(fleet.sentMessage.images) != 1 {
		t.Fatalf("sent message = %#v", fleet.sentMessage)
	}
}

func TestMessageEndpointRejectsOverLimitEpilogue(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)
	contentType, prefix := multipartMessage(t, "hello")
	body := append(append([]byte(nil), prefix...), bytes.Repeat([]byte("e"), maxMessageMultipartBytes-len(prefix)+1)...)
	wantBody := `{"accepted":false,"error":"The image attachment limits were exceeded."}`

	for _, test := range []struct {
		name          string
		contentLength int64
		chunked       bool
	}{
		{name: "known content length", contentLength: int64(len(body))},
		{name: "unknown content length", contentLength: -1, chunked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fleet := newStreamFakeFleet()
			handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
			r := httptest.NewRequest(http.MethodPost, "/ui/sessions/session-1/messages", bytes.NewReader(body))
			r.Host = effectiveAddrForTests
			r.Header.Set("Content-Type", contentType)
			r.ContentLength = test.contentLength
			if test.chunked {
				r.TransferEncoding = []string{"chunked"}
			}
			r.AddCookie(cookie)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)

			if response.Code != http.StatusRequestEntityTooLarge || response.Body.String() != wantBody {
				t.Fatalf("response = %d %q, want %d %q", response.Code, response.Body.String(), http.StatusRequestEntityTooLarge, wantBody)
			}
			if fleet.sentMessage.sessionID != "" {
				t.Fatalf("manager called for over-limit epilogue: %#v", fleet.sentMessage)
			}
		})
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("over-limit epilogue handling created temporary files: %v", entries)
	}
}

func TestMessageEndpointAcceptsBoundedEpilogue(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
	contentType, prefix := multipartMessage(t, "hello")
	body := append(append([]byte(nil), prefix...), []byte("small legal multipart epilogue")...)

	response := serveMultipartMessage(t, handler, cookie, "session-1", contentType, body)
	if response.Code != http.StatusAccepted || response.Body.String() != `{"accepted":true}` {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if fleet.sentMessage.sessionID != "session-1" || fleet.sentMessage.message != "hello" {
		t.Fatalf("sent message = %#v", fleet.sentMessage)
	}
}

func TestMessageEndpointMapsDecodeFailures(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        []byte
		wantStatus  int
		wantBody    string
	}{
		{name: "malformed", contentType: "multipart/form-data; boundary=missing", body: []byte("broken"), wantStatus: http.StatusBadRequest, wantBody: `{"accepted":false,"error":"The message upload was not valid."}`},
		{name: "limits", wantStatus: http.StatusRequestEntityTooLarge, wantBody: `{"accepted":false,"error":"The image attachment limits were exceeded."}`},
		{name: "unsupported", wantStatus: http.StatusUnsupportedMediaType, wantBody: `{"accepted":false,"error":"Only PNG, JPEG, GIF, and WebP images are supported."}`},
	}
	for i := range tests {
		test := &tests[i]
		switch test.name {
		case "limits":
			test.contentType, test.body = multipartMessage(t, "inspect", multipartFixture{name: "large.png", data: pngImage(attachments.MaxImageBytes + 1)})
		case "unsupported":
			test.contentType, test.body = multipartMessage(t, "inspect", multipartFixture{name: "fake.png", data: []byte("not an image")})
		}
		t.Run(test.name, func(t *testing.T) {
			fleet := newStreamFakeFleet()
			handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
			response := serveMultipartMessage(t, handler, cookie, "session-1", test.contentType, test.body)
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody {
				t.Fatalf("response = %d %q, want %d %q", response.Code, response.Body.String(), test.wantStatus, test.wantBody)
			}
			if fleet.sentMessage.sessionID != "" {
				t.Fatalf("manager called for rejected request: %#v", fleet.sentMessage)
			}
		})
	}
}

func TestMessageEndpointMapsManagerFailuresWithoutLeak(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{name: "image unsupported", err: manager.ErrImageInputUnsupported, wantStatus: http.StatusConflict, wantBody: `{"accepted":false,"error":"The selected model does not support image input."}`},
		{name: "unavailable", err: errors.New("private upstream failure"), wantStatus: http.StatusServiceUnavailable, wantBody: `{"accepted":false,"error":"The message could not be sent."}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			fleet := newStreamFakeFleet()
			fleet.messageErr = test.err
			handler, cookie := mustNewHandlerWithFleetAuthLogger(t, fleet, slog.New(slog.NewTextHandler(&logs, nil)))
			filename := "SENSITIVE-FILENAME.png"
			payloadMarker := "SENSITIVE-MULTIPART-MARKER"
			data := append(pngImage(16), []byte(payloadMarker)...)
			contentType, body := multipartMessage(t, "inspect", multipartFixture{name: filename, data: data})
			response := serveMultipartMessage(t, handler, cookie, "session-1", contentType, body)
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), test.err.Error()) {
				t.Fatalf("response leaked manager error: %q", response.Body.String())
			}
			if strings.Contains(logs.String(), test.err.Error()) || strings.Contains(logs.String(), "private upstream failure") {
				t.Fatalf("log leaked manager failure: %q", logs.String())
			}
			if test.wantStatus == http.StatusServiceUnavailable {
				for _, marker := range []string{"category=manager_rejection", "method=POST", "path=/ui/sessions/session-1/messages", "sessionID=session-1"} {
					if !strings.Contains(logs.String(), marker) {
						t.Fatalf("log omitted %q context: %q", marker, logs.String())
					}
				}
			}
			if strings.Contains(logs.String(), filename) || strings.Contains(logs.String(), payloadMarker) {
				t.Fatalf("log leaked multipart data or filename: %q", logs.String())
			}
		})
	}
}

func TestMessageEndpointRequiresAuthenticationAndSameOrigin(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
	contentType, body := multipartMessage(t, "hello")

	unauthenticated := serveMultipartMessage(t, handler, nil, "session-1", contentType, body)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticated.Code)
	}

	r := httptest.NewRequest(http.MethodPost, "/ui/sessions/session-1/messages", bytes.NewReader(body))
	r.Host = effectiveAddrForTests
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Origin", "http://attacker.example")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", w.Code)
	}
}

func TestMessageEndpointContentTypeIsRouteSpecific(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
	multipartType, multipartBody := multipartMessage(t, "hello")

	jsonMessage := serveMultipartMessage(t, handler, cookie, "session-1", "application/json", []byte(`{"message":"hello"}`))
	if jsonMessage.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("JSON /messages status = %d, want 415", jsonMessage.Code)
	}

	r := httptest.NewRequest(http.MethodPost, "/ui/sessions/session-1/steer", bytes.NewReader(multipartBody))
	r.Host = effectiveAddrForTests
	r.Header.Set("Content-Type", multipartType)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("multipart /steer status = %d, want 415", w.Code)
	}
}

func TestMessageEndpointTrustedNetworkStillValidatesMultipart(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, err := newHandlerWithOptions(
		slog.New(slog.NewTextHandler(io.Discard, nil)), effectiveAddrForTests, io.Discard, fleet, t.Context(), false,
	)
	if err != nil {
		t.Fatal(err)
	}

	invalid := serveMultipartMessage(t, handler, nil, "session-1", "application/json", []byte(`{"message":"hello"}`))
	if invalid.Code != http.StatusBadRequest || invalid.Body.String() != `{"accepted":false,"error":"The message upload was not valid."}` {
		t.Fatalf("trusted-network invalid response = %d %q", invalid.Code, invalid.Body.String())
	}

	contentType, body := multipartMessage(t, "hello")
	valid := serveMultipartMessage(t, handler, nil, "session-1", contentType, body)
	if valid.Code != http.StatusAccepted || valid.Body.String() != `{"accepted":true}` {
		t.Fatalf("trusted-network valid response = %d %q", valid.Code, valid.Body.String())
	}
}

func TestMultipartMessageNeverCreatesTemporaryFiles(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	for _, fixture := range []multipartFixture{
		{name: "valid.png", data: pngImage(32)},
		{name: "oversized.png", data: pngImage(attachments.MaxImageBytes + 1)},
	} {
		contentType, body := multipartMessage(t, "inspect", fixture)
		_ = serveMultipartMessage(t, handler, cookie, "session-1", contentType, body)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, filepath.Join(tmpDir, entry.Name()))
		}
		t.Fatalf("multipart handling created temporary files: %v", names)
	}
}
