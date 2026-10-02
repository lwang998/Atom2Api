//go:build nogithub

package main

// Distributions built with `-tags nogithub` carry no GitHub address: empty
// values disable the online update check and hide the repository row.
var (
	githubLatestReleaseURL = ""
	githubRepositoryURL    = ""
)
