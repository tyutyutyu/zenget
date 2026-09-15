package cmd

import (
	"errors"
	"fmt"
	"testing"
)

func TestCommandExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: 0},
		{name: "updates available", err: errUpdatesAvailable, want: 3},
		{name: "wrapped updates available", err: fmt.Errorf("check: %w", errUpdatesAvailable), want: 3},
		{name: "indeterminate", err: errUpdateCheckIndeterminate, want: 1},
		{name: "ordinary error", err: errors.New("failed"), want: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := commandExitCode(test.err); got != test.want {
				t.Errorf("commandExitCode(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}
