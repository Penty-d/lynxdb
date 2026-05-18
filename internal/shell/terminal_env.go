package shell

import "os"

// bubbleTeaEnv returns a local environment for Bubble Tea shell surfaces.
// The TERM overrides avoid Bubble Tea's 2026/2027 mode probes, whose delayed
// terminal replies can leak into scrollback after the shell exits.
func bubbleTeaEnv() []string {
	env := os.Environ()
	env = append(env,
		"TERM=xterm-256color",
		"TERM_PROGRAM=Apple_Terminal",
	)

	return env
}
