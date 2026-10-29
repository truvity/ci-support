// Package buildkit renders a BuildKit daemon for a machine of its own, with
// no provider in it: the daemon's configuration (buildkitd.toml: garbage
// collection, parallelism, the mutual-TLS listener, the debug address), its
// systemd unit with memory limits, the install of a pinned BuildKit release
// and rootlesskit, an nftables rule that keeps every
// process but root away from the instance metadata service.
//
// The daemon runs rootless, as the in-cluster builders do: as the user
// `buildkit` inside rootlesskit's user namespace, so a build step that
// escapes its container lands as that unprivileged user, never as root. A
// build step shares the host's network (rootlesskit --net=host), which is
// why the metadata service is fenced by uid and not by the hop limit alone.
//
// The result is a github.com/truvity/asg module: fragments for the
// user-data (bootstrap) of a single instance that is started and stopped
// from outside: the unit is enabled and starts on every boot. The cache
// volume is not prepared here (asg's datavolume module mounts it at
// CacheDir). A provider package (buildkit/ec2) composes it with the
// certificate module.
package buildkit

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/module"
)

// Pinned releases. A bump is by hand: the version and the release assets'
// sha256 below move together (Renovate cannot compute the digests). It
// changes the rendered user-data, so the boxes roll.
const (
	// BuildKitVersion matches the in-cluster builders' moby/buildkit image.
	BuildKitVersion = "v0.33.0"
	// RootlessKitVersion is the one moby/buildkit's Dockerfile pins for
	// that release.
	RootlessKitVersion = "v3.0.1"
)

// Paths and names on the machine.
const (
	User         = "buildkit"
	ConfigPath   = "/etc/buildkit/buildkitd.toml"
	Unit         = "buildkitd.service"
	GuardUnit    = "buildkit-imds-guard.service"
	SocketDir    = "/run/buildkit"
	Socket       = "unix://" + SocketDir + "/buildkitd.sock"
	CacheDir     = "/var/lib/buildkit"
	CacheLabel   = "bkcache"
	HomeDir      = "/var/lib/buildkit-home"
	releaseTar   = "/usr/local/src/buildkit.tar.gz"
	rootlessTar  = "/usr/local/src/rootlesskit.tar.gz"
	guardPath    = "/etc/buildkit/imds-guard.nft"
	sysctlPath   = "/etc/sysctl.d/90-buildkit.conf"
	subIDStart   = 300000
	subIDCount   = 65536
	userNSMax    = 63359
	defaultDebug = "127.0.0.1:6060"
)

// Releases are the pinned downloads per architecture ("amd64", "arm64").
// The digests are the GitHub release assets' sha256.
var (
	BuildKitRelease = map[string]bootstrap.Source{
		"amd64": {
			URL:    "https://github.com/moby/buildkit/releases/download/" + BuildKitVersion + "/buildkit-" + BuildKitVersion + ".linux-amd64.tar.gz",
			SHA256: "b6242896d343100808dcbe37565caf381e0a444a6a83d7255926bb1519248ead",
		},
		"arm64": {
			URL:    "https://github.com/moby/buildkit/releases/download/" + BuildKitVersion + "/buildkit-" + BuildKitVersion + ".linux-arm64.tar.gz",
			SHA256: "e5acfb5929f967fde3b925ddb39f79fd481a0e96774c641fab3a0e83950d7bfa",
		},
	}
	RootlessKitRelease = map[string]bootstrap.Source{
		"amd64": {
			URL:    "https://github.com/rootless-containers/rootlesskit/releases/download/" + RootlessKitVersion + "/rootlesskit-x86_64.tar.gz",
			SHA256: "0850aa446151dfbdca15ed228ff0151751792cb5a99260b9a6738e1b490cc37b",
		},
		"arm64": {
			URL:    "https://github.com/rootless-containers/rootlesskit/releases/download/" + RootlessKitVersion + "/rootlesskit-aarch64.tar.gz",
			SHA256: "fdd9d2aa12bb8914081dfe1cd129c5a6a06cf1f80c732713f905a92c07b9b45c",
		},
	}
)

var sizeRe = regexp.MustCompile(`^[0-9]+(GB|MB|%)$`)

type (
	// Config is the daemon.
	Config struct {
		// Port is the mutual-TLS gRPC listener. Default 1234.
		Port int
		// MaxParallelism caps the steps the daemon runs at once. Required:
		// an uncapped daemon takes the whole machine under a burst.
		MaxParallelism int
		// GC bounds the cache. The zero value is the default below.
		GC GC
		// TLS are the files the daemon serves and verifies clients by
		// (asg's ssmcert module writes them). Required.
		TLS TLS
		// DebugAddress serves /metrics and pprof. Default 127.0.0.1:6060
		// (loopback only).
		DebugAddress string
		// MemoryHighPercent and MemoryMaxPercent are the unit's MemoryHigh
		// and MemoryMax, as a share of the machine's memory, so the daemon
		// is throttled and then killed and restarted before it can starve
		// the machine's own health path. Default 85 and 92.
		MemoryHighPercent, MemoryMaxPercent int
		// CPUWeight is the unit's CPU weight (others default to 100), so
		// sshd and the agents win under a burst. Default 50.
		CPUWeight int
		// BootUnits are the units the daemon requires and starts
		// after, besides the metadata guard (the certificate fetch).
		BootUnits []string
	}

	// GC is the cache's garbage collection (buildkitd.toml, worker.oci).
	// Sizes are like "10GB" or "80%".
	GC struct {
		// ReservedSpace is kept even when unused. Default 10GB.
		ReservedSpace string
		// MaxUsedSpace is the most the cache may use. Default 80GB.
		MaxUsedSpace string
		// MinFreeSpace is kept free on the volume. Default 10GB.
		MinFreeSpace string
	}

	// TLS are absolute paths of the server certificate, its key and the CA
	// the daemon accepts client certificates from.
	TLS struct {
		Cert, Key, CA string
	}
)

func (c Config) withDefaults() Config {
	if c.Port == 0 {
		c.Port = 1234
	}

	if c.DebugAddress == "" {
		c.DebugAddress = defaultDebug
	}

	if c.MemoryHighPercent == 0 {
		c.MemoryHighPercent = 85
	}

	if c.MemoryMaxPercent == 0 {
		c.MemoryMaxPercent = 92
	}

	if c.CPUWeight == 0 {
		c.CPUWeight = 50
	}

	if c.GC.ReservedSpace == "" {
		c.GC.ReservedSpace = "10GB"
	}

	if c.GC.MaxUsedSpace == "" {
		c.GC.MaxUsedSpace = "80GB"
	}

	if c.GC.MinFreeSpace == "" {
		c.GC.MinFreeSpace = "10GB"
	}

	return c
}

var (
	pathRe  = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	debugRe = regexp.MustCompile(`^127\.0\.0\.1:[0-9]{2,5}$`)
)

func (c Config) validate() error {
	var errs []error

	add := func(format string, v ...any) { errs = append(errs, fmt.Errorf(format, v...)) }

	if c.Port < 1024 || c.Port > 65535 {
		add("Port %d: want 1024..65535 (the daemon is not root)", c.Port)
	}

	if c.MaxParallelism < 1 {
		add("MaxParallelism %d: want at least 1; an uncapped daemon takes the machine under a burst", c.MaxParallelism)
	}

	for what, v := range map[string]string{"ReservedSpace": c.GC.ReservedSpace, "MaxUsedSpace": c.GC.MaxUsedSpace, "MinFreeSpace": c.GC.MinFreeSpace} {
		if !sizeRe.MatchString(v) {
			add("GC.%s %q: want a size such as 10GB or 80%%", what, v)
		}
	}

	for what, p := range map[string]string{"Cert": c.TLS.Cert, "Key": c.TLS.Key, "CA": c.TLS.CA} {
		if !pathRe.MatchString(p) {
			add("TLS.%s %q: want an absolute path", what, p)
		}
	}

	if !debugRe.MatchString(c.DebugAddress) {
		add("DebugAddress %q: want 127.0.0.1:<port>; the debug endpoint serves pprof and must not leave the machine", c.DebugAddress)
	}

	if c.MemoryHighPercent < 50 || c.MemoryMaxPercent > 98 || c.MemoryHighPercent >= c.MemoryMaxPercent {
		add("MemoryHighPercent %d and MemoryMaxPercent %d: want 50 <= high < max <= 98", c.MemoryHighPercent, c.MemoryMaxPercent)
	}

	if c.CPUWeight < 1 || c.CPUWeight > 10000 {
		add("CPUWeight %d: want 1..10000", c.CPUWeight)
	}

	return errors.Join(errs...)
}

// TOML renders buildkitd.toml.
func (c Config) TOML() (string, error) {
	c = c.withDefaults()
	if err := c.validate(); err != nil {
		return "", err
	}

	return fmt.Sprintf(`# Rendered by github.com/truvity/ci-support/pkg/buildkit. No secret.
root = %q

[grpc]
  # TLS applies to every tcp:// listener; the unix socket stays plain and is
  # reachable by root only (the watchdog and the ready check).
  address = ["tcp://0.0.0.0:%d", %q]
  debugAddress = %q

  [grpc.tls]
    cert = %q
    key = %q
    ca = %q

[worker.oci]
  enabled = true
  rootless = true
  snapshotter = "overlayfs"
  max-parallelism = %d
  gc = true
  reservedSpace = %q
  maxUsedSpace = %q
  minFreeSpace = %q

[worker.containerd]
  enabled = false
`, CacheDir, c.Port, Socket, c.DebugAddress, c.TLS.Cert, c.TLS.Key, c.TLS.CA, c.MaxParallelism,
		c.GC.ReservedSpace, c.GC.MaxUsedSpace, c.GC.MinFreeSpace), nil
}

// UnitText renders buildkitd.service.
func (c Config) UnitText() string {
	c = c.withDefaults()

	// The unit starts on every boot: it is enabled, it requires the guard and the
	// BootUnits, and it hands the mount point of the data volume to its user
	// before it starts. The data volume itself is required by a drop-in of asg's
	// datavolume module.
	deps := strings.Join(append([]string{GuardUnit}, c.BootUnits...), " ")

	return fmt.Sprintf(`[Unit]
Description=BuildKit daemon (rootless, mutual TLS on :%d)
Wants=network-online.target
After=network-online.target %[2]s
Requires=%[2]s

[Service]
Type=simple
User=%[3]s
Group=%[3]s
Environment=HOME=%[4]s XDG_RUNTIME_DIR=%[5]s
RuntimeDirectory=buildkit
RuntimeDirectoryMode=0755
ExecStartPre=+/usr/bin/chown %[3]s:%[3]s %[6]s
ExecStartPre=+/usr/bin/chmod 0700 %[6]s
ExecStart=/usr/local/bin/rootlesskit --state-dir=%[6]s/rootlesskit --net=host /usr/local/bin/buildkitd --config=%[7]s
Delegate=yes
KillMode=mixed
TimeoutStopSec=120
Restart=always
RestartSec=5
LimitNOFILE=1048576
TasksMax=infinity
MemoryHigh=%[8]d%%
MemoryMax=%[9]d%%
CPUWeight=%[10]d

[Install]
WantedBy=multi-user.target
`, c.Port, deps, User, HomeDir, SocketDir, CacheDir, ConfigPath, c.MemoryHighPercent, c.MemoryMaxPercent, c.CPUWeight)
}

// GuardRules is the nftables table that drops traffic to the instance
// metadata service from every uid but root: the daemon and its build steps
// run as `buildkit`, and a build must never reach the instance's role.
const GuardRules = `# Rendered by github.com/truvity/ci-support/pkg/buildkit.
table inet buildkit_imds {
  chain output {
    type filter hook output priority filter; policy accept;
    meta skuid 0 accept
    ip daddr 169.254.169.254 counter drop
    ip6 daddr fd00:ec2::254 counter drop
  }
}
`

const guardUnit = `[Unit]
Description=Keep everything but root away from the instance metadata service
DefaultDependencies=no
Before=network-pre.target ` + Unit + `
Wants=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=-/usr/sbin/nft delete table inet buildkit_imds
ExecStart=/usr/sbin/nft -f ` + guardPath + `
ExecStop=/usr/sbin/nft delete table inet buildkit_imds

[Install]
WantedBy=multi-user.target
`

// installScript creates the user and unpacks the releases. The cache volume
// is not prepared here: the datavolume module does it on every boot.
func installScript() string {
	return fmt.Sprintf(installHead, User, HomeDir, subIDStart, subIDCount, releaseTar, rootlessTar)
}

const installHead = `set -euo pipefail
if ! id %[1]s >/dev/null 2>&1; then
  useradd -K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0 --system --create-home --home-dir %[2]s --shell /sbin/nologin %[1]s
fi
grep -q '^%[1]s:' /etc/subuid || echo '%[1]s:%[3]d:%[4]d' >>/etc/subuid
grep -q '^%[1]s:' /etc/subgid || echo '%[1]s:%[3]d:%[4]d' >>/etc/subgid
tar -xzf %[5]s -C /usr/local bin
tar -xzf %[6]s -C /usr/local/bin rootlesskit rootlessctl
rm -f %[5]s %[6]s
sysctl --system >/dev/null
`

// ReadyCheck is the shell check that the daemon answers on its unix socket.
func ReadyCheck() string {
	return "timeout 10 /usr/local/bin/buildctl --addr " + Socket + " debug workers >/dev/null\n"
}

type mod struct{ c Config }

// New returns the daemon as a github.com/truvity/asg module.
func New(c Config) module.Module { return mod{c: c} }

func (mod) Name() string { return "buildkit" }

func (m mod) Contribute() (module.Contribution, error) {
	c := m.c.withDefaults()

	sysctl := fmt.Sprintf("# Rendered by github.com/truvity/ci-support/pkg/buildkit: rootless BuildKit.\nuser.max_user_namespaces = %d\n", userNSMax)

	toml, err := c.TOML()
	if err != nil {
		return module.Contribution{}, err
	}

	frag := bootstrap.Compose("buildkit",
		bootstrap.Package{Name: "git"},
		bootstrap.Package{Name: "nftables"},
		bootstrap.Package{Name: "shadow-utils"},
		bootstrap.Package{Name: "xfsprogs"},
		bootstrap.Binary{Name: "buildkit " + BuildKitVersion, Dest: releaseTar, PerArch: BuildKitRelease},
		bootstrap.Binary{Name: "rootlesskit " + RootlessKitVersion, Dest: rootlessTar, PerArch: RootlessKitRelease},
		bootstrap.File{Path: ConfigPath, Content: toml, Delivery: bootstrap.DeliveryInline},
		bootstrap.File{Path: guardPath, Content: GuardRules, Delivery: bootstrap.DeliveryInline},
		bootstrap.File{Path: sysctlPath, Content: sysctl, Delivery: bootstrap.DeliveryInline},
		bootstrap.Step{Name: "buildkit install", Script: installScript()},
		bootstrap.Unit{Name: GuardUnit, Content: guardUnit, Enable: true, Start: true},
		bootstrap.Unit{Name: Unit, Content: c.UnitText(), Enable: true, Start: true},
	)

	return module.Contribution{
		Fragments: []bootstrap.Fragment{frag},
		Scrape:    []module.ScrapeTarget{{Name: "buildkitd", Address: c.DebugAddress, Path: "/metrics"}},
	}, nil
}

// Arch is the BuildKit name of an architecture given as EC2 names it.
func Arch(ec2Arch string) (string, error) {
	switch strings.ToLower(ec2Arch) {
	case "arm64", "aarch64":
		return "arm64", nil
	case "amd64", "x86_64":
		return "amd64", nil
	}

	return "", fmt.Errorf("architecture %q: want arm64 or amd64", ec2Arch)
}
