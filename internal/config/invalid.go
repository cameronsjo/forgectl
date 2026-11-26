package config

import "errors"

// invalidValueError is a config value that decoded and is not valid, such as
// a log_level the logger does not know. It is a type of its own so the loader
// can word the file as invalid, and not as one that does not parse.
type invalidValueError struct {
	message string
}

func (e invalidValueError) Error() string { return e.message }

// isInvalidValueError reports whether err is a refused config value.
func isInvalidValueError(err error) bool {
	var refused invalidValueError
	return errors.As(err, &refused)
}
