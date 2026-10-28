package ec2

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/asg"
	"github.com/truvity/asg/bootstrap"
	"github.com/truvity/asg/front"
	"github.com/truvity/asg/module"
	"github.com/truvity/asg/modules/datavolume"
	"github.com/truvity/asg/modules/ssmcert"

	"github.com/truvity/ci-support/pkg/buildkit"
)

// Defaults of the on-demand mode.
const (
	DefaultDataVolumeGiB = 100
	// DefaultRoleTagKey is the tag the wake proxy selects a builder by; its
	// value is the builder's Name, which is unique per environment.
	DefaultRoleTagKey = "role"
	// CertUnit fetches the certificate from SSM on every boot.
	CertUnit = "buildkit-cert.service"
	// CertCheckTimer checks the certificate parameters for a new version.
	CertCheckTimer = "buildkit-cert-check.timer"
	// CertName is the name the ssmcert module is given (it names its files
	// and units).
	certName = "buildkit"
)

// OnDemandArgs are the settings of ModeOnDemand: one plain instance that a
// wake proxy starts when a client connects and stops after it was idle.
type OnDemandArgs struct {
	// SubnetID is the instance's subnet and Zone its availability zone,
	// which is also the cache volume's: an EBS volume attaches inside one
	// zone. asg checks at deploy time that the subnet is in Zone and fails
	// the update if not. Required.
	SubnetID pulumi.StringInput
	Zone     string
	// ProxySecurityGroupID is the wake proxy's security group (the proxy's
	// SecurityGroupID output): the only source the builder's security group
	// admits on the daemon's port. Required.
	ProxySecurityGroupID pulumi.StringInput

	// DataVolumeGiB is the persistent cache volume. Default 100.
	DataVolumeGiB int
	// DataIOPS and DataThroughputMiBps raise the gp3 baseline (3000 IOPS,
	// 125 MiB/s); 0 keeps it.
	DataIOPS, DataThroughputMiBps int
	// DataKMSKeyARN encrypts the volume with that customer-managed key; empty
	// uses the account's default EBS key. Pass the same ARN to the proxy
	// (ProxyArgs.KMSKeyARNs), or starting the instance never leaves pending.
	DataKMSKeyARN string

	// RoleTagKey and RoleTagValue are the tag the proxy finds the instance by.
	// Defaults: "role" and the builder's Name.
	RoleTagKey, RoleTagValue string
}

var (
	zoneRe   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9][a-z]$`)
	tagKeyRe = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,128}$`)
	tagValRe = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+@-]{1,256}$`)
	kmsARNRe = regexp.MustCompile(`^arn:[a-z-]+:kms:[a-z0-9-]+:[0-9]{12}:key/[A-Za-z0-9-]+$`)
)

func (a Args) withOnDemandDefaults(name string) Args {
	if a.Name == "" {
		a.Name = name
	}

	if a.RootVolumeGiB == 0 {
		a.RootVolumeGiB = DefaultRootVolumeGiB
	}

	o := OnDemandArgs{}
	if a.OnDemand != nil {
		o = *a.OnDemand
	}

	if o.DataVolumeGiB == 0 {
		o.DataVolumeGiB = DefaultDataVolumeGiB
	}

	if o.RoleTagKey == "" {
		o.RoleTagKey = DefaultRoleTagKey
	}

	if o.RoleTagValue == "" {
		o.RoleTagValue = a.Name
	}

	a.OnDemand = &o

	return a
}

// validateOnDemand refuses what has no meaning without a group, so that a
// copied group configuration cannot be silently ignored.
func (a Args) validateOnDemand(raw Args) error {
	var errs []error

	add := func(format string, v ...any) { errs = append(errs, fmt.Errorf(format, v...)) }

	if raw.OnDemand == nil {
		add("OnDemand is required with Mode %q", ModeOnDemand)

		return errors.Join(errs...)
	}

	for what, set := range map[string]bool{
		"Min, Max and Desired": raw.Min != 0 || raw.Max != 0 || raw.Desired != 0,
		"WarmPoolSize":         raw.WarmPoolSize != nil,
		"SubnetIDs":            len(raw.SubnetIDs) > 0,
		"NLB":                  raw.NLB != (front.SharedNLB{}),
		"Listener":             raw.Listener != "",
		"Sources":              len(raw.Sources) > 0,
		"CacheVolumeGiB":       raw.CacheVolumeGiB != 0,
		"Watchdog":             raw.Watchdog != nil,
	} {
		if set {
			add("%s is refused with Mode %q: there is no group, and the listener belongs to the proxy (OnDemand.DataVolumeGiB sizes the cache)", what, ModeOnDemand)
		}
	}

	if _, ok := imageParameters[a.Arch]; !ok {
		add("Arch %q: want amd64 or arm64", a.Arch)
	}

	if !nameRe.MatchString(a.Name) {
		add("Name %q: want lower-case letters, digits and -, at most 48", a.Name)
	}

	if a.InstanceType == "" {
		add("InstanceType is required")
	}

	if a.VPCID == nil {
		add("VPCID is required")
	}

	if a.Port < 1024 || a.Port > 65535 {
		add("Port %d: want the listener port from the port registry (1024..65535: it is also the proxy's listen port)", a.Port)
	}

	if a.RootVolumeGiB < 8 {
		add("RootVolumeGiB %d: want at least 8", a.RootVolumeGiB)
	}

	errs = append(errs, validateCertificate(a.Certificate))

	o := a.OnDemand

	if o.SubnetID == nil {
		add("OnDemand.SubnetID is required")
	}

	if !zoneRe.MatchString(o.Zone) {
		add("OnDemand.Zone %q: want a zone such as eu-west-1a", o.Zone)
	}

	if o.ProxySecurityGroupID == nil {
		add("OnDemand.ProxySecurityGroupID is required: the builder admits the proxy's security group only")
	}

	if o.DataVolumeGiB < 20 || o.DataVolumeGiB > 16384 {
		add("OnDemand.DataVolumeGiB %d: want 20..16384", o.DataVolumeGiB)
	}

	if o.DataIOPS < 0 || o.DataThroughputMiBps < 0 {
		add("OnDemand.DataIOPS and DataThroughputMiBps must not be negative")
	}

	if o.DataKMSKeyARN != "" && !kmsARNRe.MatchString(o.DataKMSKeyARN) {
		add("OnDemand.DataKMSKeyARN %q: want one key ARN", o.DataKMSKeyARN)
	}

	if !tagKeyRe.MatchString(o.RoleTagKey) || strings.HasPrefix(strings.ToLower(o.RoleTagKey), "aws:") {
		add("OnDemand.RoleTagKey %q: want a tag key of letters, digits and _ . : / = + @ - space, not starting with aws:", o.RoleTagKey)
	}

	if !tagValRe.MatchString(o.RoleTagValue) {
		add("OnDemand.RoleTagValue %q: want a tag value of letters, digits and _ . : / = + @ - space", o.RoleTagValue)
	}

	r := a.Rotation
	if r.Name != "" || r.Cert != "" || r.Key != "" || r.CA != "" || len(r.RestartUnits) > 0 || r.ReadyCheck != "" ||
		r.DrainPort != 0 || r.Group != "" || r.Hook != "" || r.Dir != "" || r.Owner != "" {
		add("Rotation: only the timing (CheckIntervalMinutes, WindowStartHour, WindowEndHour, IdleMinutes, " +
			"ReadyTimeoutSeconds) can be set; the rest is fixed by the builder")
	}

	return errors.Join(errs...)
}

func validateCertificate(c Certificate) error {
	var errs []error

	if c.Cert == "" || c.Key == "" || c.ClientCA == "" {
		errs = append(errs, errors.New("Certificate.Cert, Key and ClientCA are required parameter names"))
	}

	if !aliasRe.MatchString(c.KeyAlias) {
		errs = append(errs, fmt.Errorf("Certificate.KeyAlias %q: want alias/<name>", c.KeyAlias))
	}

	return errors.Join(errs...)
}

// newOnDemand creates the builder of ModeOnDemand: one plain instance with a
// persistent cache volume and nothing in front of it; the proxy owns its run
// state and the NLB.
func newOnDemand(ctx *pulumi.Context, name string, args *Args, opts ...pulumi.ResourceOption) (*Builder, error) {
	a := args.withOnDemandDefaults(name)
	if err := a.validateOnDemand(*args); err != nil {
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): %w", name, err)
	}

	o := a.OnDemand

	rendered, mods, err := userDataOnDemand(a)
	if err != nil {
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): %w", name, err)
	}

	data := &asg.DataVolumeArgs{
		AvailabilityZone: o.Zone, DeviceName: DefaultCacheDevice, SizeGiB: o.DataVolumeGiB, Type: "gp3",
		IOPS: o.DataIOPS, ThroughputMiBps: o.DataThroughputMiBps, KMSKeyID: o.DataKMSKeyARN,
	}

	box, err := asg.New(ctx, name, &asg.Args{
		Names:    asg.Names{Base: a.Name},
		Tags:     a.Tags,
		Provider: a.Provider,
		UserData: rendered,
		Image:    asg.ImageArgs{SSMParameter: imageParameters[a.Arch]},
		Instance: asg.InstanceArgs{
			Type:       a.InstanceType,
			HopLimit:   1,
			RootVolume: &asg.RootVolumeArgs{SizeGiB: a.RootVolumeGiB},
		},
		Single: &asg.SingleArgs{SubnetID: o.SubnetID, RoleTagKey: o.RoleTagKey, RoleTagValue: o.RoleTagValue},
		Data:   data,
		IAM: asg.IAMArgs{
			PermissionsBoundary: a.PermissionsBoundary,
			StatementsFunc:      func(pc asg.PolicyContext) []asg.Statement { return []asg.Statement{decrypt(pc, a.Certificate)} },
		},
		Security: asg.SecurityArgs{
			VPCID:       a.VPCID,
			Description: a.Name + " BuildKit builder",
			Style:       asg.RulesSeparate,
			Ingress: []asg.Rule{{
				Description:  "BuildKit from the wake proxy",
				FromPort:     DaemonPort,
				ToPort:       DaemonPort,
				SourceGroups: []pulumi.StringInput{o.ProxySecurityGroupID},
			}},
		},
		Contribution: mods,
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("buildkit ec2: New(%q, ...): %w", name, err)
	}

	return &Builder{Box: box, Rendered: rendered, RoleTagKey: o.RoleTagKey, RoleTagValue: o.RoleTagValue}, nil
}

// InstanceID is the builder's instance (ModeOnDemand only).
func (b *Builder) InstanceID() pulumi.StringOutput { return b.Box.Instance.ID().ToStringOutput() }

// DataVolumeID is the cache volume (ModeOnDemand only).
func (b *Builder) DataVolumeID() pulumi.StringOutput { return b.Box.DataVolume.ID().ToStringOutput() }

// SecurityGroupID is the builder's security group.
func (b *Builder) SecurityGroupID() pulumi.StringOutput {
	return b.Box.SecurityGroup.ID().ToStringOutput()
}

// userDataOnDemand composes the daemon, the data volume and the certificate
// of a builder that boots on every wake. No module here has a lifecycle:
// the plan is rendered without bootstrap.Options.Lifecycle, as Single
// requires.
func userDataOnDemand(a Args) (*bootstrap.Rendered, asg.Contribution, error) {
	d := a.Daemon
	d.TLS = buildkit.TLS{Cert: certFile("cert"), Key: certFile("key"), CA: certFile("ca")}
	d.Port = DaemonPort
	d.OnDemand = true
	d.BootUnits = []string{CertUnit}

	mods, err := asg.Collect(
		buildkit.New(d),
		datavolume.New(datavolume.Args{
			MountPoint: buildkit.CacheDir, DeviceName: DefaultCacheDevice, Label: buildkit.CacheLabel,
			Units: []string{buildkit.Unit},
		}),
		bootCert(a),
	)
	if err != nil {
		return nil, asg.Contribution{}, err
	}

	p, err := bootstrap.New(bootstrap.Options{})
	if err != nil {
		return nil, asg.Contribution{}, err
	}

	if err := mods.AddTo(p); err != nil {
		return nil, asg.Contribution{}, err
	}

	rendered, err := p.Render()
	if err != nil {
		return nil, asg.Contribution{}, err
	}

	return rendered, mods, nil
}

// certFile is where ssmcert writes a file of the builder's certificate.
func certFile(kind string) string {
	cert, key, ca := ssmcert.Args{Name: certName}.Files()

	return map[string]string{"cert": cert, "key": key, "ca": ca}[kind]
}

// certRotation is the certificate module of an on-demand builder: asg's
// ssmcert module with its in-place rotation, minus everything that belongs to
// a launch lifecycle.
//
// What asg's module does with a group (an in-service step that loads the
// files and one that starts the timer) cannot run on a single instance, and
// is replaced by two units that need no hook:
//
//   - CertUnit, a oneshot that runs the module's own `load` on EVERY boot,
//     before the daemon (the daemon requires it): the instance boots on every
//     wake, so the files in /run (tmpfs, empty after a stop) are always the
//     current parameter versions, and a certificate that was renewed while
//     the instance slept needs no rotation at all;
//   - CertCheckTimer, enabled, that runs the module's own `check` service
//     hourly: on a new parameter version it rotates in place, inside the
//     window, once the daemon's port has had no established connection for
//     IdleMinutes (a running build keeps one), by loading the files and
//     restarting the daemon.
//
// A box that is never idle keeps its certificate until it is: the proxy
// stops an idle box after 20 minutes anyway, and the next wake loads the
// current version.
type certRotation struct{ rot ssmcert.Args }

func bootCert(a Args) module.Module {
	rot := a.Rotation
	rot.Name = certName
	rot.Cert, rot.Key, rot.CA = a.Certificate.Cert, a.Certificate.Key, a.Certificate.ClientCA
	rot.Owner = buildkit.User + ":" + buildkit.User
	rot.RestartUnits = []string{buildkit.Unit}
	rot.ReadyCheck = buildkit.ReadyCheck()
	rot.DrainPort = DaemonPort
	rot.InPlace = true

	// The daemon is idle-checked, not window-bound: a box that is up and idle
	// may rotate at any hour but the last (ssmcert refuses an empty window).
	if rot.WindowStartHour == nil && rot.WindowEndHour == nil {
		start, end := 0, 23
		rot.WindowStartHour, rot.WindowEndHour = &start, &end
	}

	return certRotation{rot: rot}
}

func (certRotation) Name() string { return "buildkit-cert" }

func (m certRotation) Contribute() (module.Contribution, error) {
	c, err := ssmcert.New(m.rot).Contribute()
	if err != nil {
		return module.Contribution{}, err
	}

	// Nothing of the launch lifecycle: the units below do what the steps did.
	c.InService = nil

	script := module.LibDir + "/ssmcert-" + certName + ".sh"
	conf := "/etc/asg/ssmcert-" + certName + ".conf"
	check := "asg-ssmcert-" + certName + ".service"

	load := "[Unit]\nDescription=Fetch the BuildKit server certificate, key and client CA from SSM\n" +
		"Wants=network-online.target\nAfter=network-online.target\n\n" +
		"[Service]\nType=oneshot\nRemainAfterExit=yes\nEnvironment=ASG_SSMCERT_CONF=" + conf + "\n" +
		"ExecStart=" + script + " load\nRestart=on-failure\nRestartSec=15\nTimeoutStartSec=300\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"

	every := m.rot.CheckIntervalMinutes
	if every == 0 {
		every = ssmcert.DefaultCheckIntervalMinutes
	}

	timer := fmt.Sprintf("[Unit]\nDescription=Check the BuildKit certificate parameters for a new version every %d minutes\n\n"+
		"[Timer]\nOnBootSec=%dmin\nOnUnitActiveSec=%dmin\nRandomizedDelaySec=300\nUnit=%s\n\n[Install]\nWantedBy=timers.target\n",
		every, every, every, check)

	c.Fragments = append(c.Fragments, bootstrap.Compose("buildkit-cert-boot",
		bootstrap.Unit{Name: CertUnit, Content: load, Enable: true},
		bootstrap.Unit{Name: CertCheckTimer, Content: timer, Enable: true, Start: true},
	))

	return c, nil
}
