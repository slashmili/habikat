package git

func ExportRemote() Remote {
	return Remote{
		config:      append(hostConfig(), "core.fileMode=true", "gc.auto=0", "maintenance.auto=false"),
		environment: append(commonEnvironment(), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "LC_ALL=C"),
	}
}
