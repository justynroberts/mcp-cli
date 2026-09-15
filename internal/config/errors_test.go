package config

import "errors"

func asErr(err error, target **ErrNoConfig) bool { return errors.As(err, target) }
