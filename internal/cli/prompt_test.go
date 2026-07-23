package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestPromptStringReturnsDefaultOnEmpty(t *testing.T) {
	r := strings.NewReader("\n")
	var out bytes.Buffer
	got, err := promptStringReader(&out, bufio.NewReader(r), "Label", "/default/path")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/default/path" {
		t.Errorf("got %q, want %q", got, "/default/path")
	}
}

func TestPromptStringReturnsInput(t *testing.T) {
	r := strings.NewReader("/custom/path\n")
	var out bytes.Buffer
	got, _ := promptStringReader(&out, bufio.NewReader(r), "Label", "/default/path")
	if got != "/custom/path" {
		t.Errorf("got %q, want %q", got, "/custom/path")
	}
}

func TestPromptConfirmDefaultsNo(t *testing.T) {
	r := strings.NewReader("\n")
	var out bytes.Buffer
	got, _ := promptConfirmReader(&out, bufio.NewReader(r), "Confirm?", false)
	if got != false {
		t.Errorf("got %v, want false", got)
	}
}

func TestPromptConfirmYes(t *testing.T) {
	r := strings.NewReader("y\n")
	var out bytes.Buffer
	got, _ := promptConfirmReader(&out, bufio.NewReader(r), "Confirm?", false)
	if got != true {
		t.Errorf("got %v, want true", got)
	}
}

func TestPromptConfirmYesFull(t *testing.T) {
	r := strings.NewReader("yes\n")
	var out bytes.Buffer
	got, _ := promptConfirmReader(&out, bufio.NewReader(r), "Confirm?", false)
	if got != true {
		t.Errorf("got %v, want true", got)
	}
}

func TestParseAgentsFlag(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"none", nil},
		{"claude-code", []string{"claude-code"}},
		{"claude-code,hermes", []string{"claude-code", "hermes"}},
		{"claude-code, hermes , opencode", []string{"claude-code", "hermes", "opencode"}},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseAgentsFlag(tt.in)
			if len(got) != len(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
