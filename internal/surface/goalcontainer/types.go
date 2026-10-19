package goalcontainer

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	Mode          = "container"
	HelperPath    = "/usr/local/bin/forgectl"
	Protocol      = "forgectl-goal-container-v1"
	apiVersion    = "/v1.51"
	leaseInterval = 500 * time.Millisecond
	leaseTimeout  = 3 * time.Second
	maxOutput     = 4 * 1024 * 1024
)

var imagePattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var identityPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Spec struct {
	Mode   string `json:"mode"`
	Socket string `json:"socket"`
	Image  string `json:"image"`
}

func (spec Spec) Validate() error {
	if spec.Mode != Mode {
		return errors.New("isolation mode must be container; omit isolation for existing process-group cleanup")
	}
	if !cleanPath(spec.Socket) || len(spec.Socket) > 103 {
		return errors.New("container socket must be an absolute clean local Unix path of at most 103 bytes")
	}
	if !imagePattern.MatchString(spec.Image) {
		return errors.New("container image must be an immutable local sha256 image ID")
	}
	return nil
}

func cleanPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && len(path) <= 4096 && !strings.ContainsAny(path, "\x00\r\n")
}

type Metadata struct {
	EngineID      string `json:"engine_id"`
	EngineVersion string `json:"engine_version"`
	Architecture  string `json:"architecture"`
	ImageID       string `json:"image_id"`
}

type bindOptions struct {
	Propagation  string `json:"Propagation"`
	NonRecursive bool   `json:"NonRecursive"`
}

type mount struct {
	Type        string      `json:"Type"`
	Source      string      `json:"Source"`
	Target      string      `json:"Target"`
	ReadOnly    bool        `json:"ReadOnly"`
	BindOptions bindOptions `json:"BindOptions"`
}

type restartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

type policy struct {
	NetworkMode     string            `json:"NetworkMode"`
	ReadonlyRootfs  bool              `json:"ReadonlyRootfs"`
	CapDrop         []string          `json:"CapDrop"`
	CapAdd          []string          `json:"CapAdd"`
	SecurityOpt     []string          `json:"SecurityOpt"`
	PidsLimit       int64             `json:"PidsLimit"`
	Memory          int64             `json:"Memory"`
	MemorySwap      int64             `json:"MemorySwap"`
	NanoCpus        int64             `json:"NanoCpus"`
	RestartPolicy   restartPolicy     `json:"RestartPolicy"`
	Tmpfs           map[string]string `json:"Tmpfs"`
	Mounts          []mount           `json:"Mounts"`
	CgroupnsMode    string            `json:"CgroupnsMode"`
	IpcMode         string            `json:"IpcMode"`
	PidMode         string            `json:"PidMode"`
	UTSMode         string            `json:"UTSMode"`
	UsernsMode      string            `json:"UsernsMode"`
	Privileged      bool              `json:"Privileged"`
	PublishAllPorts bool              `json:"PublishAllPorts"`
	AutoRemove      bool              `json:"AutoRemove"`
	Binds           []string          `json:"Binds"`
	VolumesFrom     []string          `json:"VolumesFrom"`
}

type healthcheck struct {
	Test []string `json:"Test"`
}

type createRequest struct {
	Image        string            `json:"Image"`
	User         string            `json:"User"`
	Entrypoint   []string          `json:"Entrypoint"`
	Cmd          []string          `json:"Cmd"`
	Env          []string          `json:"Env"`
	WorkingDir   string            `json:"WorkingDir"`
	AttachStdin  bool              `json:"AttachStdin"`
	AttachStdout bool              `json:"AttachStdout"`
	AttachStderr bool              `json:"AttachStderr"`
	OpenStdin    bool              `json:"OpenStdin"`
	StdinOnce    bool              `json:"StdinOnce"`
	Tty          bool              `json:"Tty"`
	Healthcheck  healthcheck       `json:"Healthcheck"`
	Labels       map[string]string `json:"Labels"`
	HostConfig   policy            `json:"HostConfig"`
}

type containerState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	Restarting bool   `json:"Restarting"`
	Dead       bool   `json:"Dead"`
	OOMKilled  bool   `json:"OOMKilled"`
	PID        int    `json:"Pid"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
}

type inspection struct {
	ID         string         `json:"Id"`
	Name       string         `json:"Name"`
	Image      string         `json:"Image"`
	Config     createRequest  `json:"Config"`
	HostConfig policy         `json:"HostConfig"`
	State      containerState `json:"State"`
	Mounts     []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
		Propagation string `json:"Propagation"`
	} `json:"Mounts"`
}

type Receipt struct {
	Version       int            `json:"version"`
	Name          string         `json:"name"`
	Role          string         `json:"role"`
	Nonce         string         `json:"nonce"`
	ImageID       string         `json:"image_id"`
	ContainerID   string         `json:"container_id,omitempty"`
	ConfigSHA256  string         `json:"config_sha256"`
	CommandSHA256 string         `json:"command_sha256"`
	Phase         string         `json:"phase"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	Policy        policy         `json:"policy"`
	Stopped       bool           `json:"stopped"`
	Removed       bool           `json:"removed"`
	State         containerState `json:"state"`
	Error         string         `json:"error,omitempty"`
}
