package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDispatchNoArgsPrintsHelp(t *testing.T) {
	var out bytes.Buffer
	code := Dispatch(context.Background(), nil, &out)
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected help output, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "sshmng mcp") {
		t.Errorf("expected 'sshmng mcp' in help, got: %s", out.String())
	}
}

func TestDispatchHelpFlagsPrintHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			code := Dispatch(context.Background(), []string{arg}, &out)
			if code != 0 {
				t.Errorf("code = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "Usage:") {
				t.Errorf("expected help output for %q, got: %s", arg, out.String())
			}
		})
	}
}

func TestDispatchUnknownCommandErrors(t *testing.T) {
	var out bytes.Buffer
	code := Dispatch(context.Background(), []string{"foobar"}, &out)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "Unknown command") {
		t.Errorf("expected 'Unknown command' error, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "sshmng help") {
		t.Errorf("expected hint to run 'sshmng help', got: %s", out.String())
	}
}
