package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rossoctl/cortex/authbridge/authlib/config"

	"github.com/rossoctl/rossoctl-cli/internal/containers"
)

// configWithPluginURLs builds a config whose inbound stage carries one plugin per
// given raw-JSON config body.
//
// The config is written as YAML and read back through config.Load, so the plugin
// bodies go through the same PluginEntry decoding as in production — a helper that
// set PluginEntry.Config directly could pass while the real YAML path failed.
func configWithPluginURLs(t *testing.T, pluginConfigs ...string) *config.Config {
	t.Helper()

	type plugin struct {
		Name   string `yaml:"name"`
		Config any    `yaml:"config"`
	}
	var plugins []plugin
	for i, raw := range pluginConfigs {
		var body any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("plugin config %d is not JSON: %v", i, err)
		}
		plugins = append(plugins, plugin{Name: "jwt-validation", Config: body})
	}

	doc := map[string]any{
		"mode": "proxy-sidecar",
		"listener": map[string]any{
			"forward_proxy_addr":    ":8081",
			"reverse_proxy_addr":    ":8000",
			"reverse_proxy_backend": "http://127.0.0.1:8001",
		},
		"pipeline": map[string]any{
			"inbound": map[string]any{"plugins": plugins},
		},
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("marshaling test config: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading test config: %v", err)
	}
	return cfg
}

// TestContainerCADir verifies which configs get a host CA directory mounted in.
// The decision matters both ways: without a mount a generated CA is unreachable
// by the child, and with one an operator-supplied CA would be shadowed by an
// empty temp directory.
func TestContainerCADir(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "generate_ca mounts ca_dir",
			yaml: "mode: proxy-sidecar\ntls_bridge:\n  mode: enabled\n  ca_dir: /etc/authbridge/ca\n  generate_ca: true\n",
			want: "/etc/authbridge/ca",
		},
		{
			// The CA is the operator's own material, already at ca_dir inside the
			// image or mounted by them; an empty temp dir would hide it.
			name: "operator-supplied CA is not mounted",
			yaml: "mode: proxy-sidecar\ntls_bridge:\n  mode: enabled\n  ca_dir: /etc/authbridge/ca\n  generate_ca: false\n",
			want: "",
		},
		{
			name: "no tls_bridge block",
			yaml: "mode: proxy-sidecar\n",
			want: "",
		},
		{
			// Nothing terminates TLS, so no CA is involved at all.
			name: "disabled bridge",
			yaml: "mode: proxy-sidecar\ntls_bridge:\n  mode: disabled\n  ca_dir: /etc/authbridge/ca\n  generate_ca: true\n",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, tc.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, ok := containerCADir(cfg)
			if got != tc.want || ok != (tc.want != "") {
				t.Errorf("containerCADir = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.want != "")
			}
		})
	}
}

// TestWaitForCACertAppears verifies the wait returns the path once the container
// writes the certificate, which is the normal (slightly delayed) case.
func TestWaitForCACertAppears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, caCertFileName)

	// Write it shortly after the wait starts, as the container would.
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
			t.Errorf("WriteFile: %v", err)
		}
	}()

	got, err := waitForCACert(context.Background(), dir, io.Discard)
	if err != nil {
		t.Fatalf("waitForCACert: %v", err)
	}
	if got != path {
		t.Errorf("waitForCACert = %q, want %q", got, path)
	}
}

// TestWaitForCACertIgnoresEmptyFile verifies an empty ca.crt does not satisfy the
// wait. The bridge creates the file before writing it, so returning early would
// point the child at a truncated certificate and fail every handshake.
func TestWaitForCACertIgnoresEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, caCertFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A short deadline stands in for the 30s budget: the point is that an empty
	// file times out rather than returning.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	if _, err := waitForCACert(ctx, dir, io.Discard); err == nil {
		t.Fatal("expected a timeout while ca.crt is still empty")
	}

	// And once it has content, the same directory succeeds.
	if err := os.WriteFile(path, []byte("cert"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := waitForCACert(context.Background(), dir, io.Discard); err != nil {
		t.Errorf("waitForCACert after write: %v", err)
	}
}

// TestWaitForCACertTimeoutMentionsPath verifies the timeout error names the file
// and the likely cause, since a ca_dir mismatch between the config and the mount
// is the way this fails in practice.
func TestWaitForCACertTimeoutMentionsPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	dir := t.TempDir()
	_, err := waitForCACert(ctx, dir, io.Discard)
	if err == nil {
		t.Fatal("expected a timeout for a directory that never gets a CA")
	}
	if !strings.Contains(err.Error(), caCertFileName) || !strings.Contains(err.Error(), "ca_dir") {
		t.Errorf("error %q should name %s and ca_dir", err, caCertFileName)
	}
}

// TestProxyContainerImageDefaultsOff verifies the flag defaults to empty, which
// is what keeps exec's in-process path the default behavior.
func TestProxyContainerImageDefaultsOff(t *testing.T) {
	f := authbridgeExecCmd.Flags().Lookup("proxyContainerImage")
	if f == nil {
		t.Fatal("--proxyContainerImage is not registered")
	}
	if f.DefValue != "" {
		t.Errorf("--proxyContainerImage default = %q, want empty", f.DefValue)
	}
}

// TestShortID verifies IDs are abbreviated the way the container CLIs display
// them, and that a short ID is passed through rather than sliced out of range.
func TestShortID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"abc123def4567890abcdef", "abc123def456"},
		{"abc123def456", "abc123def456"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := shortID(tc.in); got != tc.want {
			t.Errorf("shortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// fakeResolve installs a resolver answering from table for the duration of the
// test, so host-entry tests do not depend on the DNS of the machine they run on.
// A name absent from the table fails to resolve.
func fakeResolve(t *testing.T, table map[string][]string) {
	t.Helper()

	prev := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = prev })

	lookupIPAddr = func(_ context.Context, host string) ([]net.IPAddr, error) {
		ips, ok := table[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		out := make([]net.IPAddr, 0, len(ips))
		for _, s := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(s)})
		}
		return out, nil
	}
}

// TestProxyContainerHostEntriesMapsLoopbackOnly verifies that a config hostname
// resolving to loopback is mapped to the host, and one resolving elsewhere is
// left alone.
//
// Both halves matter. Without the mapping, a pipeline pointed at a Keycloak on
// this host connects to the container itself; with it applied indiscriminately, a
// pipeline pointed at a real remote Keycloak would be redirected to the host and
// break.
func TestProxyContainerHostEntriesMapsLoopbackOnly(t *testing.T) {
	fakeResolve(t, map[string][]string{
		"keycloak.localtest.me": {"127.0.0.1"},
		"keycloak.corp.example": {"10.2.3.4"},
	})

	cfg := configWithPluginURLs(t,
		`{"keycloak_url":"http://keycloak.localtest.me:8080/","issuer":"http://keycloak.localtest.me:8080/realms/r"}`,
		`{"keycloak_url":"https://keycloak.corp.example/"}`)

	entries, err := proxyContainerHostEntries(cfg, io.Discard)
	if err != nil {
		t.Fatalf("proxyContainerHostEntries: %v", err)
	}

	want := []containers.HostEntry{{Name: "keycloak.localtest.me", Address: containers.HostGateway}}
	if len(entries) != len(want) || entries[0] != want[0] {
		t.Fatalf("entries = %+v, want %+v", entries, want)
	}
}

// TestProxyContainerHostEntriesDedupes verifies a hostname appearing in several
// plugin fields yields one entry. Two --add-host flags for the same name are
// accepted by the runtimes but describe the same mapping twice.
func TestProxyContainerHostEntriesDedupes(t *testing.T) {
	fakeResolve(t, map[string][]string{"kc.local": {"127.0.0.1"}})

	cfg := configWithPluginURLs(t,
		`{"keycloak_url":"http://kc.local:8080/","issuer":"http://kc.local:8080/realms/r"}`,
		`{"keycloak_url":"http://kc.local:8080/"}`)

	entries, err := proxyContainerHostEntries(cfg, io.Discard)
	if err != nil {
		t.Fatalf("proxyContainerHostEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "kc.local" {
		t.Fatalf("entries = %+v, want one entry for kc.local", entries)
	}
}

// TestProxyContainerHostEntriesUnresolvableIsError verifies an unresolvable
// hostname fails the command rather than being skipped. It cannot be classified,
// and silently omitting it would surface as a container failing every token
// operation.
func TestProxyContainerHostEntriesUnresolvableIsError(t *testing.T) {
	fakeResolve(t, map[string][]string{})

	cfg := configWithPluginURLs(t, `{"keycloak_url":"http://typo.invalid:8080/"}`)

	_, err := proxyContainerHostEntries(cfg, io.Discard)
	if err == nil {
		t.Fatal("an unresolvable hostname should be an error")
	}
	if !strings.Contains(err.Error(), "typo.invalid") {
		t.Errorf("error %q should name the hostname that failed", err)
	}
}

// TestProxyContainerHostEntriesIgnoresNonURLs verifies plugin config values that
// are not absolute URLs produce no entries and no lookups. "passthrough" and
// "client-secret" are real values from the weather example, and treating either
// as a hostname would fail the command.
func TestProxyContainerHostEntriesIgnoresNonURLs(t *testing.T) {
	fakeResolve(t, map[string][]string{})

	cfg := configWithPluginURLs(t,
		`{"default_policy":"passthrough","identity":{"type":"client-secret"},"keycloak_realm":"rossoctl"}`)

	entries, err := proxyContainerHostEntries(cfg, io.Discard)
	if err != nil {
		t.Fatalf("proxyContainerHostEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none", entries)
	}
}

// TestConfigHostnamesFindsNestedAndListedURLs verifies hostnames are collected
// from wherever they sit in a plugin's config. The schema is the plugin's own, so
// nothing here can assume a flat shape.
func TestConfigHostnamesFindsNestedAndListedURLs(t *testing.T) {
	cfg := configWithPluginURLs(t,
		`{"nested":{"deep":{"url":"http://a.example/x"}},"list":["http://b.example","not-a-url"]}`)

	got := configHostnames(cfg)
	want := []string{"b.example", "a.example"} // "list" sorts before "nested"
	if len(got) != len(want) {
		t.Fatalf("configHostnames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("configHostnames() = %v, want %v (order must be deterministic)", got, want)
		}
	}
}

// TestResolvesToLoopbackIPLiterals verifies an IP literal is classified without a
// lookup: it cannot be an /etc/hosts entry, and the resolver here always fails,
// so a lookup would turn a valid config into an error.
func TestResolvesToLoopbackIPLiterals(t *testing.T) {
	fakeResolve(t, map[string][]string{})

	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.53", true},
		{"::1", true},
		{"10.2.3.4", false},
		{"8.8.8.8", false},
	} {
		got, err := resolvesToLoopback(tc.ip)
		if err != nil {
			t.Errorf("resolvesToLoopback(%q): %v", tc.ip, err)
			continue
		}
		if got != tc.want {
			t.Errorf("resolvesToLoopback(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestResolvesToLoopbackMixedAddresses verifies a name resolving to both loopback
// and a routable address counts as loopback: the container cannot reach the
// loopback half, which is where a local service listens.
func TestResolvesToLoopbackMixedAddresses(t *testing.T) {
	fakeResolve(t, map[string][]string{"mixed.local": {"10.2.3.4", "127.0.0.1"}})

	got, err := resolvesToLoopback("mixed.local")
	if err != nil {
		t.Fatalf("resolvesToLoopback: %v", err)
	}
	if !got {
		t.Error("a name with any loopback address should count as loopback")
	}
}

// portConfig builds a config from a raw listener/stats YAML body and applies the
// preset, matching what startHost does before the container path reads ports.
func portConfig(t *testing.T, body string) *config.Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("loading test config: %v", err)
	}
	config.ApplyPreset(cfg)
	return cfg
}

// TestContainerPortsFromConfig verifies the published ports come from the config
// rather than from constants, which is the whole point: the image binds what its
// config says, so a hardcoded port is published on nothing.
func TestContainerPortsFromConfig(t *testing.T) {
	cfg := portConfig(t, `mode: proxy-sidecar
listener:
  forward_proxy_addr: :19081
  reverse_proxy_addr: 0.0.0.0:19000
  reverse_proxy_backend: http://127.0.0.1:8001
  session_api_addr: 127.0.0.1:19094
stats:
  address: :19093
`)

	got, err := containerPortsFromConfig(cfg)
	if err != nil {
		t.Fatalf("containerPortsFromConfig: %v", err)
	}
	want := containerPorts{reverse: 19000, forward: 19081, admin: 19093, sessionAPI: 19094}
	if got != want {
		t.Fatalf("ports = %+v, want %+v", got, want)
	}

	// publishList feeds -p directly, so its contents and order are what the
	// runtime sees.
	if list, wantList := got.publishList(), []int{19000, 19081, 19093, 19094}; !slices.Equal(list, wantList) {
		t.Errorf("publishList() = %v, want %v", list, wantList)
	}
}

// TestContainerPortsFromConfigUsesPresetDefaults verifies a config that omits its
// addresses gets the ports ApplyPreset filled in. This is the case the previous
// hardcoded 8000 got wrong: the preset gives the reverse proxy :8080, so
// publishing 8000 would have mapped a host port to nothing.
func TestContainerPortsFromConfigUsesPresetDefaults(t *testing.T) {
	cfg := portConfig(t, `mode: proxy-sidecar
listener:
  reverse_proxy_backend: http://127.0.0.1:8001
`)

	got, err := containerPortsFromConfig(cfg)
	if err != nil {
		t.Fatalf("containerPortsFromConfig: %v", err)
	}
	want := containerPorts{reverse: 8080, forward: 8081, admin: 9093, sessionAPI: 9094}
	if got != want {
		t.Fatalf("ports = %+v, want the preset defaults %+v", got, want)
	}
}

// TestContainerPortsFromConfigSkipsInactiveRole verifies an inactive role's port
// is not published. ApplyPreset fills an address only for an active role, and
// publishing one the image will not bind maps a host port to nothing.
func TestContainerPortsFromConfigSkipsInactiveRole(t *testing.T) {
	cfg := portConfig(t, `mode: proxy-sidecar
listener:
  roles: [forward]
`)

	got, err := containerPortsFromConfig(cfg)
	if err != nil {
		t.Fatalf("containerPortsFromConfig: %v", err)
	}
	if got.reverse != 0 {
		t.Errorf("reverse = %d, want 0 for a forward-only config", got.reverse)
	}
	if got.forward != 8081 {
		t.Errorf("forward = %d, want 8081", got.forward)
	}
	for _, p := range got.publishList() {
		if p == 0 {
			t.Error("publishList must not contain a zero port")
		}
	}
}

// TestPublishListSkipsZeroPorts verifies a port left at zero is dropped rather
// than passed to -p. Zero means "no listener", and publishing it would either be
// rejected by the runtime or map a host port to nothing.
//
// Driven directly rather than through a config, because config.Load and
// ApplyPreset between them fill every address, so no loadable config produces a
// zero admin or session-API port for containerPortsFromConfig to return.
func TestPublishListSkipsZeroPorts(t *testing.T) {
	tests := []struct {
		name string
		in   containerPorts
		want []int
	}{
		{
			name: "all set keeps declaration order",
			in:   containerPorts{reverse: 1, forward: 2, admin: 3, sessionAPI: 4},
			want: []int{1, 2, 3, 4},
		},
		{
			// A forward-only config: nothing listens on the reverse port.
			name: "no reverse listener",
			in:   containerPorts{forward: 8081, admin: 9093, sessionAPI: 9094},
			want: []int{8081, 9093, 9094},
		},
		{
			name: "only the forward proxy",
			in:   containerPorts{forward: 8081},
			want: []int{8081},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.publishList()
			if !slices.Equal(got, tc.want) {
				t.Errorf("publishList() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestContainerPortsFromConfigBadAddress verifies a malformed or unpublishable
// address is an error rather than a silent skip: the operator asked for a listener
// that cannot be honored, and an unpublished port becomes a connection refused
// long after the fact.
func TestContainerPortsFromConfigBadAddress(t *testing.T) {
	for _, tc := range []struct{ name, body, wantIn string }{
		{
			name:   "not host:port",
			body:   "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: nonsense\n",
			wantIn: "forward_proxy_addr",
		},
		{
			name:   "port 0 cannot be published",
			body:   "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:0\n",
			wantIn: "port 0",
		},
		{
			name:   "non-numeric port",
			body:   "mode: proxy-sidecar\nlistener:\n  roles: [forward]\n  forward_proxy_addr: 127.0.0.1:notaport\n",
			wantIn: "invalid port",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := containerPortsFromConfig(portConfig(t, tc.body))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q should mention %q", err, tc.wantIn)
			}
		})
	}
}

// TestContainerPortsFromConfigRequiresForward verifies a config with no forward
// proxy is rejected. Pointing the child at a proxy is the purpose of hosting the
// pipeline, and catching it here costs a container start and stop less than
// discovering it after Inspect.
func TestContainerPortsFromConfigRequiresForward(t *testing.T) {
	cfg := portConfig(t, `mode: proxy-sidecar
listener:
  roles: [reverse]
  reverse_proxy_backend: http://127.0.0.1:8001
`)

	_, err := containerPortsFromConfig(cfg)
	if err == nil {
		t.Fatal("a config with no forward proxy should be rejected")
	}
	if !strings.Contains(err.Error(), "forward_proxy_addr") {
		t.Errorf("error %q should name the missing field", err)
	}
}

// TestReportContainerPorts verifies the verbose port map names each published port
// and reports a port the image did not bind as unpublished rather than omitting it.
// That gap is the interesting case: it means the image ignored its own config, and
// a silently missing line reads as "everything is fine".
func TestReportContainerPorts(t *testing.T) {
	want := containerPorts{reverse: 8000, forward: 8081, admin: 9093, sessionAPI: 9094}
	bound := map[string][]containers.PortBinding{
		"8000/tcp": {{HostIP: "127.0.0.1", HostPort: "49001"}},
		"8081/tcp": {{HostIP: "127.0.0.1", HostPort: "49002"}},
		"9094/tcp": {{HostIP: "127.0.0.1", HostPort: "49004"}},
		// 9093 deliberately absent: the admin listener never came up.
	}

	var out strings.Builder
	reportContainerPorts(&out, bound, want)
	got := out.String()

	for _, line := range []string{
		"container reverse proxy: 8000 -> 127.0.0.1:49001\n",
		"container forward proxy: 8081 -> 127.0.0.1:49002\n",
		"container admin: 9093 not published\n",
		"container session API: 9094 -> 127.0.0.1:49004\n",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("output missing %q; got:\n%s", line, got)
		}
	}
}

// TestReportContainerPortsSkipsAbsentListeners verifies a port the config never
// asked for produces no line at all. "not published" is a complaint about the
// image, and a listener that was never configured is not one.
func TestReportContainerPortsSkipsAbsentListeners(t *testing.T) {
	var out strings.Builder
	reportContainerPorts(&out, map[string][]containers.PortBinding{
		"8081/tcp": {{HostIP: "127.0.0.1", HostPort: "49002"}},
	}, containerPorts{forward: 8081})

	got := out.String()
	if strings.Contains(got, "reverse proxy") || strings.Contains(got, "admin") {
		t.Errorf("unconfigured listeners should not be reported; got:\n%s", got)
	}
	if !strings.Contains(got, "container forward proxy: 8081 -> 127.0.0.1:49002") {
		t.Errorf("output missing the forward proxy line; got:\n%s", got)
	}
}

// TestConfigHostnamesIgnoresNonDialableSchemes verifies a URL-shaped value that is
// not something to connect to contributes no hostname.
//
// The motivating case is the local weather demo's audience,
// spiffe://localtest.me/... — a SPIFFE identity whose "host" is a trust domain, not
// a server. Mapping it would add a bogus /etc/hosts entry, and a trust domain that
// does not resolve would fail the whole command under the unresolvable-name rule.
func TestConfigHostnamesIgnoresNonDialableSchemes(t *testing.T) {
	cfg := configWithPluginURLs(t, `{
		"audience": "spiffe://localtest.me/ns/team1/sa/weather-service",
		"socket": "unix:///spiffe-workload-api/spire-agent.sock",
		"keycloak_url": "http://keycloak.localtest.me:8080/",
		"issuer": "https://issuer.example/realms/x"
	}`)

	got := configHostnames(cfg)
	want := []string{"issuer.example", "keycloak.localtest.me"} // sorted by key: issuer, keycloak_url
	if !slices.Equal(got, want) {
		t.Errorf("configHostnames = %v, want %v (only http/https hosts)", got, want)
	}
}
