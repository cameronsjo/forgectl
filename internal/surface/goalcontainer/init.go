package goalcontainer

import (
	"encoding/json"
	"errors"
	"os"
)

type probe struct {
	Protocol        string `json:"protocol"`
	UID             int    `json:"uid"`
	GID             int    `json:"gid"`
	Groups          []int  `json:"groups"`
	Effective       string `json:"effective"`
	Permitted       string `json:"permitted"`
	Inherited       string `json:"inherited"`
	Ambient         string `json:"ambient"`
	NoNewPrivileges string `json:"no_new_privileges"`
	Seccomp         string `json:"seccomp"`
}

func verifyProbe(value string) error {
	return verifyProbeIdentity(value, os.Geteuid(), os.Getegid())
}

func verifyProbeIdentity(value string, uid, gid int) error {
	var actual probe
	if json.Unmarshal([]byte(value), &actual) != nil || actual.Protocol != Protocol || actual.UID != uid || actual.GID != gid || actual.UID == 0 || actual.GID == 0 || len(actual.Groups) != 0 || actual.Effective != "0000000000000000" || actual.Permitted != "0000000000000000" || actual.Inherited != "0000000000000000" || actual.Ambient != "0000000000000000" || actual.NoNewPrivileges != "1" || actual.Seccomp != "2" {
		return errors.New("container helper did not prove non-root, capability-free, no-new-privileges and seccomp execution")
	}
	return nil
}
