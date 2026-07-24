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

func TestDispatch_Update(t *testing.T) {
	var out bytes.Buffer
	code := Dispatch(context.Background(), []string{"update", "-h"}, &out)
	// -h → flag.ErrHelp → exit 0 or 2 depending on flag pkg behavior.
	// Just verify "update" is recognized (not "Unknown command").
	if strings.Contains(out.String(), "Unknown command") {
		t.Errorf("update not routed: %s", out.String())
	}
	_ = code
}

func TestDispatch_Version(t *testing.T) {
	var out bytes.Buffer
	code := Dispatch(context.Background(), []string{"version"}, &out)
	if code != 0 {
		t.Errorf("version exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "sshmng") {
		t.Errorf("version output missing sshmng: %s", out.String())
	}
}

func TestDispatch_HelpTextMentionsUpdateVersion(t *testing.T) {
	var out bytes.Buffer
	Dispatch(context.Background(), []string{}, &out)
	output := out.String()
	if !strings.Contains(output, "update") {
		t.Errorf("helpText missing 'update':\n%s", output)
	}
	if !strings.Contains(output, "version") {
		t.Errorf("helpText missing 'version':\n%s", output)
	}
}
