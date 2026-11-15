package git

func Status(extra ...string) []string {
	return append([]string{"status", "--porcelain=v1", "--untracked-files=all"}, extra...)
}

func IndexFlags(extra ...string) []string {
	return append([]string{"ls-files", "-v"}, extra...)
}

func arguments(config, args []string) []string {
	result := make([]string, 0, len(config)*2+len(args))
	for _, setting := range config {
		result = append(result, "-c", setting)
	}
	return append(result, args...)
}

func commonEnvironment() []string {
	return []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
}

func hookConfig() []string {
	return []string{"core.hooksPath=/dev/null"}
}

func inspectionConfig() []string {
	return append(hookConfig(), "core.fsmonitor=false", "core.untrackedCache=false")
}

func hostConfig() []string {
	return append(inspectionConfig(),
		"credential.helper=", "core.askPass=/bin/false", "protocol.allow=never",
		"commit.gpgSign=false", "tag.gpgSign=false", "core.attributesFile=/dev/null",
	)
}
