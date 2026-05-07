package version

import "testing"

func TestCurrentUsesLdflagsAndShortCommit(t *testing.T) {
	oldTag, oldCommit, oldDirty, oldBuildTime := Tag, Commit, Dirty, BuildTime
	t.Cleanup(func() {
		Tag, Commit, Dirty, BuildTime = oldTag, oldCommit, oldDirty, oldBuildTime
	})

	Tag = "v1.2.3"
	Commit = "0123456789abcdef"
	Dirty = "true"
	BuildTime = "2026-05-06T20:30:00Z"

	info := Current()
	if info.Tag != "v1.2.3" {
		t.Fatalf("tag = %q, want %q", info.Tag, "v1.2.3")
	}
	if info.Commit != "0123456789abcdef" {
		t.Fatalf("commit = %q, want %q", info.Commit, "0123456789abcdef")
	}
	if info.CommitShort != "0123456789ab" {
		t.Fatalf("commitShort = %q, want %q", info.CommitShort, "0123456789ab")
	}
	if info.Dirty == nil || !*info.Dirty {
		t.Fatalf("dirty = %v, want true", info.Dirty)
	}
	if info.BuildTime != "2026-05-06T20:30:00Z" {
		t.Fatalf("buildTime = %q, want %q", info.BuildTime, "2026-05-06T20:30:00Z")
	}
	if info.GoVersion == "" {
		t.Fatal("goVersion is empty")
	}
}

func TestCurrentLeavesDirtyUnsetWhenUnknown(t *testing.T) {
	oldDirty := Dirty
	t.Cleanup(func() { Dirty = oldDirty })
	Dirty = "unknown"

	info := Current()
	// info.Dirty may be set from VCS metadata (when tests run inside a git
	// checkout), but should never be a forced false-from-zero-value.
	if info.Dirty != nil && info.VCSModified == nil {
		t.Fatalf("dirty = %v with no source, want nil (unknown)", *info.Dirty)
	}
}
