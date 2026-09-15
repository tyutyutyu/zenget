package wizard

import (
	"bytes"
	"strings"
	"testing"
)

func TestSessionChooseAndAsk(t *testing.T) {
	var output bytes.Buffer
	session := New(strings.NewReader("2\ncustom\n"), &output)

	choice, err := session.Choose("Select an asset:", []string{"one", "two"})
	if err != nil {
		t.Fatalf("Choose() error = %v", err)
	}
	if choice != "two" {
		t.Fatalf("Choose() = %q, want two", choice)
	}
	name, err := session.Ask("Target binary name", "default")
	if err != nil {
		t.Fatalf("Ask() error = %v", err)
	}
	if name != "custom" {
		t.Fatalf("Ask() = %q, want custom", name)
	}
	if !strings.Contains(output.String(), "1) one") || !strings.Contains(output.String(), "2) two") {
		t.Fatalf("prompt output = %q", output.String())
	}
}

func TestChooseAndAskErrorsAndDefaults(t *testing.T) {
	if _, err := Choose(strings.NewReader("1\n"), nil, "empty", nil); err == nil {
		t.Fatal("Choose() error = nil for empty options")
	}
	if _, err := Choose(strings.NewReader("9\n"), nil, "choice", []string{"one"}); err == nil {
		t.Fatal("Choose() error = nil for out-of-range choice")
	}
	if _, err := Choose(strings.NewReader("not-a-number\n"), nil, "choice", []string{"one"}); err == nil {
		t.Fatal("Choose() error = nil for non-numeric choice")
	}
	if _, err := Ask(strings.NewReader("\n"), nil, "name", ""); err == nil {
		t.Fatal("Ask() error = nil for empty answer without default")
	}
	name, err := Ask(strings.NewReader("\n"), nil, "name", "default")
	if err != nil {
		t.Fatalf("Ask() default error = %v", err)
	}
	if name != "default" {
		t.Fatalf("Ask() default = %q, want default", name)
	}
}

func TestInteractive(t *testing.T) {
	choice, name, err := Interactive(strings.NewReader("1\n\n"), nil, "asset", []string{"one"}, "repo")
	if err != nil {
		t.Fatalf("Interactive() error = %v", err)
	}
	if choice != "one" || name != "repo" {
		t.Fatalf("Interactive() = (%q, %q), want (one, repo)", choice, name)
	}
}
