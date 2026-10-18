// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"regexp"
	"strings"
)

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellQuote writes a command line so that a POSIX shell reads back exactly
// these arguments and runs nothing else: plain arguments as they are, the
// rest in single quotes, with a single quote written as '\”.
func ShellQuote(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if plainArg.MatchString(a) {
			out[i] = a
		} else {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}

// SafeText makes text from a file or a server safe to print on a terminal:
// escape sequences and other control characters are dropped and line
// breaks become spaces.
func SafeText(s string) string {
	return strings.Join(strings.Fields(strings.Join(cleanOutput([]byte(s)), " ")), " ")
}
