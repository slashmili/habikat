package main

func runAttach(args []string, stopSignals func(), attachVM func(string) error) error {
	name, err := parseNameArg(args)
	if err != nil {
		return err
	}
	stopSignals()
	return attachVM(name)
}
