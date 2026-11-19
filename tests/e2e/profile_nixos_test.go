//go:build e2e && nixexperiment

package e2e

const e2eBuildTags = "e2e,nixexperiment"
const expectedGuestRelease = "ID=nixos"
const expectedGuestGit = "git version 2.56.0"
