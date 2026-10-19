package goalcontainer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

type client struct {
	socket    string
	transport *http.Transport
	http      *http.Client
}

type apiError struct {
	status int
}

func (err apiError) Error() string { return fmt.Sprintf("container API returned HTTP %d", err.status) }

func newClient(socket string) *client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 32 * 1024, DisableKeepAlives: true}
	return &client{socket: socket, transport: transport, http: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (api *client) request(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+apiVersion+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := api.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return err
	}
	if len(data) > 2*1024*1024 {
		return errors.New("container API response exceeds 2 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return apiError{status: response.StatusCode}
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			return errors.New("container API returned invalid JSON")
		}
	}
	return nil
}

func Inspect(ctx context.Context, spec Spec) (Metadata, error) {
	if err := spec.Validate(); err != nil {
		return Metadata{}, err
	}
	info, err := os.Lstat(spec.Socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return Metadata{}, errors.New("container runtime requires an existing local Unix socket; start/configure it explicitly, never by fallback")
	}
	api := newClient(spec.Socket)
	defer api.transport.CloseIdleConnections()
	return api.inspectMetadata(ctx, spec)
}

func (api *client) inspectMetadata(ctx context.Context, spec Spec) (Metadata, error) {
	var engine struct {
		Version      string
		OS           string `json:"Os"`
		Architecture string `json:"Arch"`
	}
	if err := api.request(ctx, http.MethodGet, "/version", nil, &engine); err != nil {
		return Metadata{}, fmt.Errorf("local Docker Engine API 1.51 is unavailable: %w", err)
	}
	var info struct {
		ID              string
		OSType          string
		CgroupVersion   string
		MemoryLimit     bool
		PidsLimit       bool
		SecurityOptions []string
	}
	if err := api.request(ctx, http.MethodGet, "/info", nil, &info); err != nil {
		return Metadata{}, err
	}
	seccomp := false
	for _, option := range info.SecurityOptions {
		if strings.HasPrefix(option, "name=seccomp,") && strings.Contains(option, "profile=builtin") {
			seccomp = true
		}
	}
	if engine.OS != "linux" || info.OSType != "linux" || info.CgroupVersion != "2" || !info.MemoryLimit || !info.PidsLimit || !seccomp || info.ID == "" {
		return Metadata{}, errors.New("container runtime requires Linux, cgroup v2, memory/PID limits and built-in seccomp")
	}
	var image struct {
		ID           string `json:"Id"`
		OS           string `json:"Os"`
		Architecture string
		Config       struct {
			Env     []string
			Volumes map[string]json.RawMessage
		}
	}
	if err := api.request(ctx, http.MethodGet, "/images/"+url.PathEscape(spec.Image)+"/json", nil, &image); err != nil {
		return Metadata{}, fmt.Errorf("approved container image must already exist locally; no pull or fallback: %w", err)
	}
	if image.ID != spec.Image || image.OS != "linux" || image.Architecture != engine.Architecture || len(image.Config.Volumes) != 0 {
		return Metadata{}, errors.New("container image identity, Linux architecture or declared volumes violate the approved boundary")
	}
	for _, entry := range image.Config.Env {
		key, _, found := strings.Cut(entry, "=")
		if !found || !slices.Contains([]string{"PATH", "HOME", "LANG", "LC_ALL", "TZ"}, key) {
			return Metadata{}, errors.New("container image has unsupported environment defaults; use a credential-free goal tool image")
		}
	}
	return Metadata{EngineID: info.ID, EngineVersion: engine.Version, Architecture: image.Architecture, ImageID: image.ID}, nil
}

func (api *client) inspect(ctx context.Context, identity string) (inspection, error) {
	var result inspection
	err := api.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(identity)+"/json", nil, &result)
	return result, err
}

func (api *client) attach(ctx context.Context, identity string) (net.Conn, *bufio.Reader, error) {
	if !identityPattern.MatchString(identity) {
		return nil, nil, errors.New("attach requires the full owned container ID")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "unix", api.socket)
	if err != nil {
		return nil, nil, err
	}
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = connection.Close()
		return nil, nil, err
	}
	request := "POST " + apiVersion + "/containers/" + identity + "/attach?stream=1&stdin=1&stdout=1&stderr=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		_ = connection.Close()
		return nil, nil, err
	}
	reader := bufio.NewReaderSize(connection, 32*1024)
	response, err := readUpgrade(reader)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(response.Header.Get("Upgrade"), "tcp") {
		_ = connection.Close()
		return nil, nil, errors.New("container attach did not establish its private upgraded stream")
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		_ = connection.Close()
		return nil, nil, err
	}
	return connection, reader, nil
}

func readUpgrade(reader *bufio.Reader) (*http.Response, error) {
	var header []byte
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil || len(header)+len(line) > 32*1024 {
			return nil, errors.New("container attach header is incomplete or exceeds 32 KiB")
		}
		header = append(header, line...)
		if bytes.Equal(line, []byte("\r\n")) {
			return http.ReadResponse(bufio.NewReader(bytes.NewReader(header)), nil)
		}
	}
}

func readOutput(reader io.Reader) (string, string, error) {
	var stdout, stderr bytes.Buffer
	for {
		var header [8]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return stdout.String(), stderr.String(), nil
			}
			return stdout.String(), stderr.String(), err
		}
		length := binary.BigEndian.Uint32(header[4:])
		if length > maxOutput {
			return stdout.String(), stderr.String(), errors.New("container output exceeds 4 MiB")
		}
		if (header[0] != 1 && header[0] != 2) || header[1] != 0 || header[2] != 0 || header[3] != 0 || stdout.Len()+stderr.Len() > maxOutput-int(length) {
			return stdout.String(), stderr.String(), errors.New("container output frame is invalid or exceeds 4 MiB")
		}
		writer := &stdout
		if header[0] == 2 {
			writer = &stderr
		}
		if _, err := io.CopyN(writer, reader, int64(length)); err != nil {
			return stdout.String(), stderr.String(), err
		}
	}
}
