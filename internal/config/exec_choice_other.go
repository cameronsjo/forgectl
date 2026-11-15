//go:build !unix

package config

// readPathTrust reads like ReadPath and trusts the file: Rex is macOS-only, and
// the owner and mode checks need a Unix stat.
func readPathTrust(path string) (data []byte, trust, err error) {
	data, err = ReadPath(path)
	return data, nil, err
}
