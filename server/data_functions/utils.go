package data_functions

// GitCommit is set at build time via -ldflags
var GitCommit = "unknown"

func GetGitCommit() string {
	return GitCommit
}
