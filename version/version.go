package version

import (
	"runtime"
	"runtime/debug"
	"strings"
)

const unknown = "unknown"

// These variables are intended to be populated by -ldflags at build time.
var (
	Tag       = unknown
	Commit    = unknown
	Dirty     = unknown
	BuildTime = unknown
)

// Info describes the build running this process.
//
// Dirty is a *bool so we can distinguish "clean" (false) from "we have no
// information" (omitted). VCSModified mirrors what Go's embedded VCS metadata
// reports independently of the consolidated Dirty value.
type Info struct {
	Tag           string `json:"tag"`
	Commit        string `json:"commit"`
	CommitShort   string `json:"commitShort,omitempty"`
	Dirty         *bool  `json:"dirty,omitempty"`
	BuildTime     string `json:"buildTime,omitempty"`
	GoVersion     string `json:"goVersion"`
	ModuleVersion string `json:"moduleVersion,omitempty"`
	VCSRevision   string `json:"vcsRevision,omitempty"`
	VCSTime       string `json:"vcsTime,omitempty"`
	VCSModified   *bool  `json:"vcsModified,omitempty"`
}

// Current returns detailed version information, using ldflags first and Go's
// embedded VCS metadata as a fallback when available.
func Current() Info {
	info := Info{
		Tag:       clean(Tag),
		Commit:    clean(Commit),
		BuildTime: clean(BuildTime),
		GoVersion: runtime.Version(),
	}
	if dirty, ok := parseBool(Dirty); ok {
		info.Dirty = &dirty
	}

	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		if v := buildInfo.Main.Version; v != "" && v != "(devel)" {
			info.ModuleVersion = v
			if info.Tag == unknown {
				info.Tag = v
			}
		}

		for _, setting := range buildInfo.Settings {
			switch setting.Key {
			case "vcs.revision":
				info.VCSRevision = setting.Value
				if info.Commit == unknown {
					info.Commit = setting.Value
				}
			case "vcs.time":
				info.VCSTime = setting.Value
				if info.BuildTime == unknown {
					info.BuildTime = setting.Value
				}
			case "vcs.modified":
				if modified, ok := parseBool(setting.Value); ok {
					info.VCSModified = &modified
					if info.Dirty == nil {
						info.Dirty = &modified
					}
				}
			}
		}
	}

	if short := shortCommit(info.Commit); short != unknown {
		info.CommitShort = short
	}
	if info.BuildTime == unknown {
		info.BuildTime = ""
	}
	return info
}

func clean(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return unknown
	}
	return value
}

func parseBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "dirty":
		return true, true
	case "false", "0", "no", "clean":
		return false, true
	default:
		return false, false
	}
}

func shortCommit(commit string) string {
	commit = clean(commit)
	if commit == unknown {
		return unknown
	}
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}
