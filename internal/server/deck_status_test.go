package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// TestDeckStatusSuccessCarriesUniqueMarker verifies each success acknowledgment
// receives a fresh, distinct marker so the client can distinguish even
// identical "Command sent." acknowledgments despite Datastar morphing the same
// DOM shape.
func TestDeckStatusSuccessCarriesUniqueMarker(t *testing.T) {
	a := newDeckStatusView(nil)
	b := newDeckStatusView(nil)
	if !a.Success {
		t.Fatalf("newDeckStatusView(nil) = %.0v, want Success", a)
	}
	if a.SuccessID == "" {
		t.Errorf("success view has empty SuccessID")
	}
	if b.SuccessID == "" {
		t.Errorf("second success view has empty SuccessID")
	}
	if a.SuccessID == b.SuccessID {
		t.Errorf("success markers not unique: both = %q", a.SuccessID)
	}
}

// TestDeckStatusErrorHasNoSuccessMarker verifies an error view never carries a
// success marker, so the client does not schedule an auto-clear for it.
func TestDeckStatusErrorHasNoSuccessMarker(t *testing.T) {
	view := newDeckStatusView(errors.New("boom"))
	if view.Success {
		t.Errorf("error view should not be Success")
	}
	if view.SuccessID != "" {
		t.Errorf("error view carried SuccessID %q", view.SuccessID)
	}
	if view.Error == "" {
		t.Errorf("error view has empty operator copy")
	}
}

// TestDeckStatusTemplateRendersSuccessMarker verifies the rendered success
// fragment embeds the unique marker on the transient success span.
func TestDeckStatusTemplateRendersSuccessMarker(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	html, err := renderTemplate(templates, templateDeckStatus, newDeckStatusView(nil))
	if err != nil {
		t.Fatalf("render deck-status.html: %v", err)
	}
	if !strings.Contains(html, "Command sent.") {
		t.Errorf("success fragment missing operator copy:\n%s", html)
	}
	if !strings.Contains(html, `class="deck-ok"`) {
		t.Errorf("success fragment missing transient .deck-ok span:\n%s", html)
	}
	if !strings.Contains(html, "data-success-id=") {
		t.Errorf("success fragment missing per-success marker:\n%s", html)
	}
}

// TestDeckStatusTemplateRendersErrorWithoutMarker verifies the error fragment
// shows operator copy but no success marker (so it is never auto-cleared).
func TestDeckStatusTemplateRendersErrorWithoutMarker(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	html, err := renderTemplate(templates, templateDeckStatus, newDeckStatusView(contract.NewError(contract.ErrorSessionStopping, "detail")))
	if err != nil {
		t.Fatalf("render deck-status.html: %v", err)
	}
	if !strings.Contains(html, "The session is already stopping.") {
		t.Errorf("error fragment missing operator copy:\n%s", html)
	}
	if !strings.Contains(html, `class="deck-error"`) {
		t.Errorf("error fragment missing .deck-error span:\n%s", html)
	}
	if strings.Contains(html, "data-success-id=") {
		t.Errorf("error fragment unexpectedly carried a success marker:\n%s", html)
	}
	if strings.Contains(html, "Command sent.") {
		t.Errorf("error fragment unexpectedly rendered success copy:\n%s", html)
	}
}
