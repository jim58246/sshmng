package cli

import "io"

// DoctorOpts will be expanded in Task 7.
type DoctorOpts struct {
	Home           string
	ExpectedBinary string
	AgentFilter    []string
}

// RunDoctor is a stub; replaced in Task 7.
func RunDoctor(opts DoctorOpts, out io.Writer) int {
	return 0
}
