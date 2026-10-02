//go:build !nogithub

package main

// Default update-check endpoints for standard builds.
var (
	githubLatestReleaseURL = "https://api.github.com/repos/lwang998/Atom2Api/releases/latest"
	githubRepositoryURL    = "https://github.com/lwang998/Atom2Api"
)
