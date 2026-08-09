package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestDispatch_File_Routing verifies 'sshmng file' dispatches to its subcommands
// and rejects unknown subcommands (mirrors cli_test.go's Dispatch pattern).
func TestDispatch_File_Routing(t *testing.T) {
	t.Run("no subcommand prints usage and exits 2", func(t *testing.T) {
		var out bytes.Buffer
		code := Dispatch(context.Background(), []string{"file"}, &out)
		if code != 2 {
			t.Errorf("code = %d, want 2", code)
		}
		if !strings.Contains(out.String(), "Usage:") {
			t.Errorf("expected usage, got: %s", out.String())
		}
		if !strings.Contains(out.String(), "sshmng file upload") {
			t.Errorf("expected upload usage line, got: %s", out.String())
		}
	})

	t.Run("unknown subcommand exits 2 with hint", func(t *testing.T) {
		var out bytes.Buffer
		code := Dispatch(context.Background(), []string{"file", "foobar"}, &out)
		if code != 2 {
			t.Errorf("code = %d, want 2", code)
		}
		if !strings.Contains(out.String(), "Unknown file subcommand") {
			t.Errorf("expected 'Unknown file subcommand', got: %s", out.String())
		}
		if !strings.Contains(out.String(), "upload") {
			t.Errorf("expected hint listing subcommands, got: %s", out.String())
		}
	})

	// Each subcommand with -h must be recognized (not "Unknown file subcommand")
	// and print its own usage line.
	for _, sub := range []string{"upload", "download", "upload-dir", "download-dir", "relay"} {
		t.Run(sub+" -h recognized", func(t *testing.T) {
			var out bytes.Buffer
			Dispatch(context.Background(), []string{"file", sub, "-h"}, &out)
			if strings.Contains(out.String(), "Unknown file subcommand") {
				t.Errorf("%s not routed: %s", sub, out.String())
			}
			if !strings.Contains(out.String(), "sshmng file "+sub) {
				t.Errorf("expected 'sshmng file %s' usage line, got: %s", sub, out.String())
			}
		})
	}
}

// TestRunFileUpload_ArgCount verifies positional arg count validation for a
// single-file subcommand (exercises the flag-parsing path without needing a
// real SSH server, same standard as ssh_cmd which has no integration tests).
func TestRunFileUpload_ArgCount(t *testing.T) {
	t.Run("too few args exits 2", func(t *testing.T) {
		var out bytes.Buffer
		code := runFileUpload([]string{"web1", "/tmp/a"}, &out)
		if code != 2 {
			t.Errorf("code = %d, want 2", code)
		}
		if !strings.Contains(out.String(), "expected 3 positional args") {
			t.Errorf("expected arg-count error, got: %s", out.String())
		}
	})

	t.Run("too many args exits 2", func(t *testing.T) {
		var out bytes.Buffer
		code := runFileUpload([]string{"web1", "/tmp/a", "/tmp/b", "/tmp/c"}, &out)
		if code != 2 {
			t.Errorf("code = %d, want 2", code)
		}
		if !strings.Contains(out.String(), "expected 3 positional args") {
			t.Errorf("expected arg-count error, got: %s", out.String())
		}
	})
}

// TestRunFileRelay_ToRequired verifies --to is mandatory and the error surfaces
// before any connection attempt.
func TestRunFileRelay_ToRequired(t *testing.T) {
	var out bytes.Buffer
	code := runFileRelay([]string{"web1", "/tmp/src", "/tmp/dst"}, &out)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "--to is required") {
		t.Errorf("expected '--to is required', got: %s", out.String())
	}
}

// TestRunFileRelay_ArgCount verifies positional arg count for relay.
func TestRunFileRelay_ArgCount(t *testing.T) {
	var out bytes.Buffer
	code := runFileRelay([]string{"web1", "/tmp/src", "/tmp/dst", "extra", "--to", "web2"}, &out)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "expected 3 positional args") {
		t.Errorf("expected arg-count error, got: %s", out.String())
	}
}

// TestRunFileRelay_ToFlagParsing verifies --to accepts both comma-separated and
// repeatable forms (pflag StringSlice behavior the relay handler relies on).
// We pass 2 positionals (3 required) so the handler passes the len(*to)==0
// check — proving --to parsed — then hits the arg-count error. If --to had
// failed to parse, we'd see "--to is required" instead.
func TestRunFileRelay_ToFlagParsing(t *testing.T) {
	for _, args := range [][]string{
		{"--to", "a,b,c", "web1", "/tmp/src"},      // comma-separated
		{"--to", "a", "--to", "b", "web1", "/tmp/src"}, // repeatable
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			code := runFileRelay(args, &out)
			if code != 2 {
				t.Errorf("code = %d, want 2", code)
			}
			if strings.Contains(out.String(), "--to is required") {
				t.Errorf("--to should have parsed; got --to-required: %s", out.String())
			}
			if !strings.Contains(out.String(), "expected 3 positional args") {
				t.Errorf("expected arg-count error after --to parsed, got: %s", out.String())
			}
		})
	}
}

// TestHelpTextMentionsFile verifies the top-level helpText lists the file
// subcommand (keeps help output in sync with Dispatch routing).
func TestHelpTextMentionsFile(t *testing.T) {
	var out bytes.Buffer
	Dispatch(context.Background(), []string{}, &out)
	output := out.String()
	if !strings.Contains(output, "sshmng file") {
		t.Errorf("helpText missing 'sshmng file':\n%s", output)
	}
	if !strings.Contains(output, "relay") {
		t.Errorf("helpText missing 'relay':\n%s", output)
	}
}
