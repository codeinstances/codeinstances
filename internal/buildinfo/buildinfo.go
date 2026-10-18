// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package buildinfo says which release and commit a binary was built from.
package buildinfo

import (
	"runtime"
	"runtime/debug"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
)

// Record is what `codeinstances version -json` prints.
type Record struct {
	Version  string `json:"version"`
	Format   string `json:"format"`
	Revision string `json:"revision"`
	Modified bool   `json:"modified"`
	Go       string `json:"go"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// Info reads the version from the package and the commit from the VCS
// details Go stamps into builds made from a git checkout.
func Info() Record {
	r := Record{Version: codeinstances.Version, Format: instance.Format, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				r.Revision = s.Value
			case "vcs.modified":
				r.Modified = s.Value == "true"
			}
		}
	}
	if r.Revision == "" {
		r.Revision = "unknown"
	}
	return r
}
