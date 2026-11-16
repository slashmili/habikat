package main

import (
	"errors"

	"github.com/slashmili/habikat/internal/vm"
)

func parseNameArg(args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New(usage)
	}
	if err := vm.ValidateName(args[0]); err != nil {
		return "", err
	}
	return args[0], nil
}
