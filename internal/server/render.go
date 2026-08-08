package server

import (
	"bytes"
	"context"
	"html/template"

	"github.com/starfederation/datastar-go/datastar"
)

// renderTemplate executes a named template into a buffer and returns the result.
// Template errors are returned before any output is written.
func renderTemplate(templates *template.Template, name string, data any) (string, error) {
	var buffer bytes.Buffer
	if err := templates.ExecuteTemplate(&buffer, name, data); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

// patchTemplate renders a named template to a buffer and patches the result
// into the SSE stream as an outer replacement of the element with the given ID.
// Template errors are returned before the SSE stream is touched.
func patchTemplate(sse *datastar.ServerSentEventGenerator, templates *template.Template, name, id string, data any) error {
	rendered, err := renderTemplate(templates, name, data)
	if err != nil {
		return err
	}
	return sse.PatchElements(rendered, datastar.WithSelectorID(id), datastar.WithModeOuter())
}

// mergeStreamContext returns a context that is canceled when either parent or serverStreams is canceled.
func mergeStreamContext(parent context.Context, serverStreams context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-ctx.Done():
		case <-serverStreams.Done():
			cancel()
		}
	}()
	return ctx, cancel
}
