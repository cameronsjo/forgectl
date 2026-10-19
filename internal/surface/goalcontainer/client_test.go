package goalcontainer

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestContainerAttachBoundsHeadersWithoutConsumingOutput(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nretained-output"))
	response, err := readUpgrade(reader)
	if err != nil || response.StatusCode != 101 {
		t.Fatalf("valid upgrade was rejected: %v", err)
	}
	remaining, err := io.ReadAll(reader)
	if err != nil || string(remaining) != "retained-output" {
		t.Fatalf("upgrade consumed tool output: %q %v", remaining, err)
	}
	for _, header := range []string{"HTTP/1.1 101 UPGRADED\r\n", "HTTP/1.1 101 UPGRADED\r\nLarge: " + strings.Repeat("x", 32768) + "\r\n\r\n", "HTTP/1.1 101 UPGRADED\r\n" + strings.Repeat("Header: value\r\n", 3000) + "\r\n"} {
		if _, err := readUpgrade(bufio.NewReader(strings.NewReader(header))); err == nil {
			t.Fatal("unbounded or incomplete upgrade was accepted")
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (transport roundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func responseJSON(status int, value any) *http.Response {
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}
}

func TestContainerSpecRequiresExplicitImmutableLocalBoundary(t *testing.T) {
	valid := Spec{Mode: Mode, Socket: "/run/user/docker.sock", Image: "sha256:" + strings.Repeat("a", 64)}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Spec){
		"mode":      func(spec *Spec) { spec.Mode = "host" },
		"remote":    func(spec *Spec) { spec.Socket = "tcp://localhost:2375" },
		"relative":  func(spec *Spec) { spec.Socket = "docker.sock" },
		"traversal": func(spec *Spec) { spec.Socket = "/run/../docker.sock" },
		"control":   func(spec *Spec) { spec.Socket = "/run/docker\nsock" },
		"long":      func(spec *Spec) { spec.Socket = "/" + strings.Repeat("a", 104) },
		"tag":       func(spec *Spec) { spec.Image = "alpine:latest" },
		"short":     func(spec *Spec) { spec.Image = "sha256:aaa" },
		"uppercase": func(spec *Spec) { spec.Image = "sha256:" + strings.Repeat("A", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := valid
			mutate(&invalid)
			if invalid.Validate() == nil {
				t.Fatal("unsafe isolation was accepted")
			}
		})
	}
}

func TestContainerMetadataRefusesMissingControlsAndImageDefaults(t *testing.T) {
	spec := Spec{Mode: Mode, Socket: "/run/docker.sock", Image: "sha256:" + strings.Repeat("a", 64)}
	for _, mode := range []string{"valid", "wrong-image", "architecture", "volume", "environment", "seccomp", "cgroups", "missing-image", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			api := &client{http: &http.Client{Transport: roundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodGet || !strings.HasPrefix(request.URL.Path, apiVersion) {
					t.Fatalf("metadata made an unexpected mutation/request: %s %s", request.Method, request.URL.Path)
				}
				switch request.URL.Path {
				case apiVersion + "/version":
					return responseJSON(200, map[string]any{"Version": "28.4.0", "Os": "linux", "Arch": "arm64"}), nil
				case apiVersion + "/info":
					security := []string{"name=seccomp,profile=builtin"}
					cgroups := "2"
					if mode == "seccomp" {
						security = nil
					}
					if mode == "cgroups" {
						cgroups = "1"
					}
					return responseJSON(200, map[string]any{"ID": "local-engine", "OSType": "linux", "CgroupVersion": cgroups, "MemoryLimit": true, "PidsLimit": true, "SecurityOptions": security}), nil
				default:
					identity, architecture := spec.Image, "arm64"
					if mode == "wrong-image" {
						identity = "sha256:" + strings.Repeat("b", 64)
					}
					if mode == "architecture" {
						architecture = "amd64"
					}
					configuration := map[string]any{"Env": []string{"PATH=/usr/bin:/bin"}}
					if mode == "volume" {
						configuration["Volumes"] = map[string]any{"/data": map[string]any{}}
					}
					if mode == "environment" {
						configuration["Env"] = []string{"UNAPPROVED=DO-NOT-PRINT"}
					}
					if mode == "missing-image" {
						return responseJSON(404, map[string]string{"message": "absent"}), nil
					}
					if mode == "malformed" {
						return responseJSON(200, "not an image"), nil
					}
					return responseJSON(200, map[string]any{"Id": identity, "Os": "linux", "Architecture": architecture, "Config": configuration}), nil
				}
			})}}
			metadata, err := api.inspectMetadata(t.Context(), spec)
			if mode == "valid" {
				if err != nil || metadata.ImageID != spec.Image || calls != 3 {
					t.Fatalf("metadata=%+v calls=%d err=%v", metadata, calls, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "DO-NOT-PRINT") {
				t.Fatalf("mode %s did not fail privately: %v", mode, err)
			}
		})
	}
}

func TestContainerOutputRequiresBoundedCompleteFrames(t *testing.T) {
	frame := func(channel byte, value string) []byte {
		length := len(value)
		if length > maxOutput {
			t.Fatal("test frame exceeds its explicit bound")
			return nil
		}
		header := []byte{channel, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(header[4:], uint32(length))
		return append(header, []byte(value)...)
	}
	valid := append(frame(1, "output"), frame(2, "error")...)
	stdout, stderr, err := readOutput(bytes.NewReader(valid))
	if err != nil || stdout != "output" || stderr != "error" {
		t.Fatalf("%q %q %v", stdout, stderr, err)
	}
	oversized := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(oversized[4:], maxOutput+1)
	for name, data := range map[string][]byte{"channel": frame(0, "input"), "truncated": valid[:len(valid)-1], "header": {1, 0}, "oversized": oversized} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := readOutput(bytes.NewReader(data)); err == nil {
				t.Fatal("malformed frame accepted")
			}
		})
	}
}

func TestContainerAPIRejectsOversizedAndErrorResponses(t *testing.T) {
	for _, status := range []int{200, 500} {
		api := &client{http: &http.Client{Transport: roundTrip(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 2*1024*1024+1)))}, nil
		})}}
		if err := api.request(t.Context(), http.MethodGet, "/info", nil, nil); err == nil {
			t.Fatal("oversized response accepted")
		}
	}
}

func TestContainerProbeRequiresActualReducedWorkloadIdentity(t *testing.T) {
	valid := probe{Protocol: Protocol, UID: 501, GID: 20, Effective: "0000000000000000", Permitted: "0000000000000000", Inherited: "0000000000000000", Ambient: "0000000000000000", NoNewPrivileges: "1", Seccomp: "2"}
	for name, mutate := range map[string]func(*probe){
		"valid":          func(_ *probe) {},
		"protocol":       func(actual *probe) { actual.Protocol = "different" },
		"root":           func(actual *probe) { actual.UID = 0 },
		"root-group":     func(actual *probe) { actual.GID = 0 },
		"groups":         func(actual *probe) { actual.Groups = []int{20} },
		"effective":      func(actual *probe) { actual.Effective = "00000000000000c0" },
		"permitted":      func(actual *probe) { actual.Permitted = "00000000000000c0" },
		"inherited":      func(actual *probe) { actual.Inherited = "00000000000000c0" },
		"ambient":        func(actual *probe) { actual.Ambient = "00000000000000c0" },
		"privilege-gain": func(actual *probe) { actual.NoNewPrivileges = "0" },
		"seccomp":        func(actual *probe) { actual.Seccomp = "0" },
	} {
		t.Run(name, func(t *testing.T) {
			actual := valid
			mutate(&actual)
			data, err := json.Marshal(actual)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyProbeIdentity(string(data), 501, 20); (err == nil) != (name == "valid") {
				t.Fatalf("unexpected proof verdict: %v", err)
			}
		})
	}
}
