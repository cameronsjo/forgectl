//go:build !unix

package config

// readPathTrust reads like ReadPath and trusts the file: the owner and mode
// checks need a Unix stat, and the executables they guard (rex, the launch
// harnesses) are run from Unix hosts.
func readPathTrust(path string) (data []byte, trust, err error) {
	data, err = ReadPath(path)
	return data, nil, err
}
