// Package wizard provides small, line-oriented terminal prompts.
package wizard

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Session keeps one buffered input reader for a sequence of prompts.
type Session struct {
	reader *bufio.Reader
	out    io.Writer
}

// New creates a prompt session using in and out.
func New(in io.Reader, out io.Writer) *Session {
	if in == nil {
		in = strings.NewReader("")
	}
	if out == nil {
		out = io.Discard
	}
	return &Session{reader: bufio.NewReader(in), out: out}
}

// Choose displays numbered options and returns the selected option. Input is
// one-based, so a user selecting the first item enters 1.
func (s *Session) Choose(prompt string, options []string) (string, error) {
	if len(options) == 0 {
		return "", fmt.Errorf("no choices available")
	}
	if _, err := fmt.Fprintln(s.out, prompt); err != nil {
		return "", err
	}
	for i, option := range options {
		if _, err := fmt.Fprintf(s.out, "%d) %s\n", i+1, option); err != nil {
			return "", err
		}
	}
	if _, err := fmt.Fprint(s.out, "Choice: "); err != nil {
		return "", err
	}

	line, err := s.reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read choice: %w", err)
	}
	choice, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || choice < 1 || choice > len(options) {
		return "", fmt.Errorf("invalid choice %q: enter a number from 1 to %d", strings.TrimSpace(line), len(options))
	}
	return options[choice-1], nil
}

// Ask prompts for a free-form value. An empty response returns defaultValue.
func (s *Session) Ask(prompt, defaultValue string) (string, error) {
	if defaultValue != "" {
		prompt = fmt.Sprintf("%s [%s]", prompt, defaultValue)
	}
	if _, err := fmt.Fprintf(s.out, "%s: ", prompt); err != nil {
		return "", err
	}
	line, err := s.reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read answer: %w", err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		answer = defaultValue
	}
	if answer == "" {
		return "", fmt.Errorf("answer cannot be empty")
	}
	return answer, nil
}

// Choose is a convenience wrapper for a single numbered prompt.
func Choose(in io.Reader, out io.Writer, prompt string, options []string) (string, error) {
	return New(in, out).Choose(prompt, options)
}

// Ask is a convenience wrapper for a single free-form prompt.
func Ask(in io.Reader, out io.Writer, prompt, defaultValue string) (string, error) {
	return New(in, out).Ask(prompt, defaultValue)
}

// Interactive selects one option and then asks for a target name. It is used
// by install when a release or archive requires user input.
func Interactive(in io.Reader, out io.Writer, prompt string, options []string, defaultName string) (choice, targetName string, err error) {
	session := New(in, out)
	choice, err = session.Choose(prompt, options)
	if err != nil {
		return "", "", err
	}
	targetName, err = session.Ask("Target binary name", defaultName)
	if err != nil {
		return "", "", err
	}
	return choice, targetName, nil
}
