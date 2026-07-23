package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// promptStringReader reads a line from r, returns input or def if empty.
// out is where the prompt label is written (os.Stdout in production).
func promptStringReader(out io.Writer, r *bufio.Reader, label, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// promptConfirmReader reads a line, returns true for y/yes, false for n/no/empty.
// out is where the prompt label is written (os.Stdout in production).
func promptConfirmReader(out io.Writer, r *bufio.Reader, label string, def bool) (bool, error) {
	defStr := "y/N"
	if def {
		defStr = "Y/n"
	}
	fmt.Fprintf(out, "%s [%s]: ", label, defStr)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	switch line {
	case "y", "yes":
		return true, nil
	case "n", "no", "":
		return def, nil
	default:
		return false, fmt.Errorf("invalid response %q (expected y/n)", line)
	}
}

// parseAgentsFlag parses --agents value into a list of Agent names.
// "none" or "" -> nil (skip Agent injection). Comma-separated otherwise.
// Whitespace around items is trimmed.
func parseAgentsFlag(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "none" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
