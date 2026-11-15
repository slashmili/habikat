package git

import "strings"

type Remote struct {
	config      []string
	environment []string
}

func ImportRemote() Remote {
	return Remote{config: hookConfig(), environment: commonEnvironment()}
}

func InspectionRemote() Remote {
	return Remote{
		config:      append(inspectionConfig(), "core.fileMode=true"),
		environment: append(commonEnvironment(), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1"),
	}
}

func (r Remote) WithConfig(setting string) Remote {
	r.config = append(append([]string{}, r.config...), setting)
	return r
}

func (r Remote) Environment() string {
	return "export " + shellWords(r.environment)
}

func (r Remote) Command(args ...string) string {
	return "git " + shellWords(arguments(r.config, args))
}

func shellWords(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = "'" + strings.ReplaceAll(word, "'", "'\\''") + "'"
	}
	return strings.Join(quoted, " ")
}
