// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package install

import (
	"context"
	_ "embed" // required for go:embed
	"encoding/csv"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/alessio/shellescape"
	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl/licenseccl"
	"github.com/cockroachdb/cockroach/pkg/roachprod/config"
	"github.com/cockroachdb/cockroach/pkg/roachprod/logger"
	"github.com/cockroachdb/cockroach/pkg/roachprod/roachprodutil"
	"github.com/cockroachdb/cockroach/pkg/roachprod/ssh"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm/gce"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/version"
	"golang.org/x/exp/maps"
)

//go:embed scripts/start.sh
var startScript string

//go:embed files/cockroachdb-logging.yaml
var loggingConfig string

func cockroachNodeBinary(c *SyncedCluster, node Node) string {
	if filepath.IsAbs(c.Binary) {
		return c.Binary
	}
	if !c.IsLocal() {
		return "./" + c.Binary
	}

	path := filepath.Join(c.localVMDir(node), c.Binary)
	if _, err := os.Stat(path); err == nil {
		return path
	}

	// For "local" clusters we have to find the binary to run and translate it to
	// an absolute path. First, look for the binary in PATH.
	path, err := exec.LookPath(c.Binary)
	if err != nil {
		if strings.HasPrefix(c.Binary, "/") {
			return c.Binary
		}
		// We're unable to find the binary in PATH and "binary" is a relative path:
		// look in the cockroach repo.
		gopath := os.Getenv("GOPATH")
		if gopath == "" {
			return c.Binary
		}
		path = gopath + "/src/github.com/cockroachdb/cockroach/" + c.Binary
		var err2 error
		path, err2 = exec.LookPath(path)
		if err2 != nil {
			return c.Binary
		}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return c.Binary
	}
	return path
}

func argExists(args []string, target string) int {
	for i, arg := range args {
		if arg == target || strings.HasPrefix(arg, target+"=") {
			return i
		}
	}
	return -1
}

// StartOpts houses the options needed by Start().
type StartOpts struct {
	Target StartTarget
	// ExtraArgs are extra arguments used when starting the node. Multiple
	// arguments should be passed as separate items in the slice. For example:
	//   Instead of: []string{"--flag foo bar"}
	//   Use:        []string{"--flag", "foo", "bar"}
	ExtraArgs []string

	// ScheduleBackups starts a backup schedule once the cluster starts
	ScheduleBackups    bool
	ScheduleBackupArgs string

	// systemd limits on resources.
	NumFilesLimit int64

	// InitTarget is the node that Start will run Init on and the default node
	// that will be used when constructing join arguments.
	InitTarget int

	// JoinTargets is the list of nodes that will be used when constructing join
	// arguments. If empty, the default is to use the InitTarget.
	JoinTargets []int

	// SQLPort is the port on which the cockroach process is listening for SQL
	// connections. Default (0) will find an open port.
	SQLPort int

	// AdminUIPort is the port on which the cockroach process is listening for
	// HTTP traffic for the Admin UI. Default (0) will find an open port.
	AdminUIPort int

	// -- Options that apply only to StartDefault target --

	SkipInit        bool
	StoreCount      int
	EncryptedStores bool
	// WALFailover, if non-empty, configures the value to supply to the
	// --wal-failover start flag.
	//
	// In a multi-store configuration, this may be set to "among-stores" to
	// enable WAL failover among stores. In a single-store configuration, this
	// should be set to `path=<path>`.
	WALFailover string
	// Populated in Start() by checking the version of the cockroach binary on the first node.
	// N.B. may be nil if the version cannot be fetched.
	Version *version.Version

	// -- Options that apply only to the StartServiceForVirtualCluster target --
	VirtualClusterName     string
	VirtualClusterID       int
	VirtualClusterLocation string // where separate process virtual clusters will be started
	SQLInstance            int
	StorageCluster         *SyncedCluster

	// IsRestart allows skipping steps that are used during initial start like
	// initialization and sequential node starts and also reuses the previous start script.
	IsRestart bool

	// EnableFluentSink determines whether to enable the fluent-servers attribute
	// in the CockroachDB logging configuration.
	EnableFluentSink bool

	// PreStartHooks are hooks that are run after service registration has occurred,
	// but before starting the cockroach process.
	PreStartHooks []PreStartHook
}

func (s *StartOpts) IsVirtualCluster() bool {
	return s.Target == StartSharedProcessForVirtualCluster || s.Target == StartServiceForVirtualCluster
}

// customPortsSpecified determines if custom ports were passed in
// the start options (via command line or otherwise).
func (s *StartOpts) customPortsSpecified() bool {
	if s.SQLPort != 0 && s.SQLPort != config.DefaultSQLPort {
		return true
	}
	if s.AdminUIPort != 0 && s.AdminUIPort != config.DefaultAdminUIPort {
		return true
	}
	return false
}

// validate checks that the start options are valid to be used in a
// cluster with the given properties. Returns an error describing the
// problem if the options are invalid.
func (s *StartOpts) validate(isLocal, supportsRegistration bool) error {
	// Local clusters do not support specifying ports. An error is returned if we
	// detect that they were set.
	if isLocal && s.customPortsSpecified() {
		return fmt.Errorf("local clusters do not support specifying ports")
	}

	if !supportsRegistration && s.customPortsSpecified() {
		return fmt.Errorf("service registration is not supported for this cluster, but custom ports were specified")
	}

	return nil
}

// StartTarget identifies what flavor of cockroach we are starting.
type StartTarget int

const (
	// StartDefault starts a "full" (KV+SQL) node (the default).
	StartDefault StartTarget = iota
	// StartSharedProcessForVirtualCluster starts an in-memory tenant
	// (shared process) for a virtual cluster.
	StartSharedProcessForVirtualCluster
	// StartServiceForVirtualCluster starts a SQL/HTTP-only server
	// process for a virtual cluster.
	StartServiceForVirtualCluster
)

const (
	// startSQLTimeout identifies the COCKROACH_CONNECT_TIMEOUT to use (in seconds)
	// for sql cmds within syncedCluster.Start().
	startSQLTimeout = 1200
	// NoSQLTimeout indicates that a `cockroach sql` call is expected to
	// succeed immediately (i.e., the server is known to be accepting
	// requests at the time the call is made).
	NoSQLTimeout = 0

	defaultInitTarget = Node(1)
)

func (st StartTarget) String() string {
	return [...]string{
		StartDefault:                        "default",
		StartSharedProcessForVirtualCluster: "shared-process virtual cluster instance",
		StartServiceForVirtualCluster:       "SQL/HTTP instance for virtual cluster",
	}[st]
}

// GetInitTarget returns the Node that should be used for
// initialization operations.
func (so StartOpts) GetInitTarget() Node {
	if so.InitTarget == 0 {
		return defaultInitTarget
	}
	return Node(so.InitTarget)
}

// GetJoinTargets returns the list of Nodes that should be used for
// join operations. If no join targets are specified, the init target
// is used.
func (so StartOpts) GetJoinTargets() []Node {
	nodes := make([]Node, len(so.JoinTargets))
	for i, n := range so.JoinTargets {
		nodes[i] = Node(n)
	}
	if len(nodes) == 0 {
		nodes = []Node{so.GetInitTarget()}
	}
	return nodes
}

// allowServiceRegistration is a gating function that prevents the usage of
// service registration, with DNS services, in scenarios where it is not
// supported. This is currently the case for non-GCE clusters and for clusters
// that are not part of the default GCE project. For custom projects the DNS
// services get garbage collected, hence the registration is not allowed.
func (c *SyncedCluster) allowServiceRegistration() bool {
	if c.IsLocal() {
		return true
	}
	for _, cVM := range c.VMs {
		if cVM.Provider != gce.ProviderName {
			return false
		}
		if cVM.Project != gce.DefaultProject() {
			return false
		}
	}
	return true
}

// maybeRegisterServices registers the SQL and Admin UI DNS services for the cluster if necessary:
//  1. The system interface is only registered if custom ports are passed in or if it
//     is running in local mode. By default, the system interface is assumed to be
//     running on the default ports.
//  2. Separate process virtual clusters are always registered. They always run on non
//     default ports as we assume those to be taken by the system interface.
//  3. Shared-process virtual clusters do not register services, as they share the same
//     ports as the system interface.
//
// If service registration is deemed necessary, ports will be selected in the following order:
//  1. If the startOpts specify non-zero ports, they will be used.
//  2. If no port is specified, a search for open ports will be performed and selected for use.
func (c *SyncedCluster) maybeRegisterServices(
	ctx context.Context, l *logger.Logger, startOpts StartOpts, portFunc FindOpenPortsFunc,
) error {
	serviceMap, err := c.MapServices(ctx, startOpts.VirtualClusterName, startOpts.SQLInstance)
	if err != nil {
		return err
	}

	var servicesToRegister ServiceDescriptors
	switch startOpts.Target {
	case StartDefault:
		// The system interface is registered only when custom ports are specified or
		// if it is a local cluster. Local clusters utilize non-default ports to prevent
		// conflicts, which necessitates explicit service registration.
		if startOpts.customPortsSpecified() || c.IsLocal() {
			startOpts.VirtualClusterName = SystemInterfaceName
			// The system interface is always regarded as a shared service, as it contains both a
			// SQL and KV server.
			servicesToRegister, err = c.servicesWithOpenPortSelection(
				ctx, l, startOpts, ServiceModeShared, serviceMap, portFunc,
			)
		}
	case StartServiceForVirtualCluster:
		// Separate process virtual clusters are always external services,
		// as they only contain a SQL server.
		servicesToRegister, err = c.servicesWithOpenPortSelection(
			ctx, l, startOpts, ServiceModeExternal, serviceMap, portFunc,
		)
	default:
		// For shared-process virtual clusters, we don't need to register
		// services as these will be resolved to the system interface process.
	}

	if err != nil {
		return err
	}
	return c.RegisterServices(ctx, servicesToRegister)
}

// servicesWithOpenPortSelection returns services to be registered for
// cases where a new cockroach process is being instantiated and needs
// to bind to available ports. This happens when we start the system
// interface process, or when we start SQL servers for separate process
// virtual clusters. If an existing service is found it is not registered again.
func (c *SyncedCluster) servicesWithOpenPortSelection(
	ctx context.Context,
	l *logger.Logger,
	startOpts StartOpts,
	serviceMode ServiceMode,
	serviceMap NodeServiceMap,
	portFunc FindOpenPortsFunc,
) (ServiceDescriptors, error) {
	var mu syncutil.Mutex
	var servicesToRegister ServiceDescriptors
	err := c.Parallel(ctx, l, WithNodes(c.Nodes), func(ctx context.Context, node Node) (*RunResultDetails, error) {
		services := make(ServiceDescriptors, 0)
		res := &RunResultDetails{Node: node}
		if _, ok := serviceMap[node][ServiceTypeSQL]; !ok {
			services = append(services, ServiceDesc{
				VirtualClusterName: startOpts.VirtualClusterName,
				ServiceType:        ServiceTypeSQL,
				ServiceMode:        serviceMode,
				Node:               node,
				Port:               startOpts.SQLPort,
				Instance:           startOpts.SQLInstance,
			})
		}
		if _, ok := serviceMap[node][ServiceTypeUI]; !ok {
			services = append(services, ServiceDesc{
				VirtualClusterName: startOpts.VirtualClusterName,
				ServiceType:        ServiceTypeUI,
				ServiceMode:        serviceMode,
				Node:               node,
				Port:               startOpts.AdminUIPort,
				Instance:           startOpts.SQLInstance,
			})
		}
		requiredPorts := 0
		for _, service := range services {
			if service.Port == 0 {
				requiredPorts++
			}
		}
		if requiredPorts > 0 {
			openPorts, err := portFunc(ctx, l, node, config.DefaultOpenPortStart, requiredPorts)
			if err != nil {
				res.Err = err
				return res, errors.Wrapf(err, "failed to find %d open ports", requiredPorts)
			}
			for idx := range services {
				if services[idx].Port != 0 {
					continue
				}
				services[idx].Port = openPorts[0]
				openPorts = openPorts[1:]
			}
		}

		mu.Lock()
		defer mu.Unlock()
		servicesToRegister = append(servicesToRegister, services...)
		return res, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find services to register: %w", err)
	}

	return servicesToRegister, nil
}

// IsExternalService is a helper that determines if a given virtual cluster
// is an external service.
func (c *SyncedCluster) IsExternalService(
	ctx context.Context, virtualClusterName string,
) (bool, error) {
	if IsSystemInterface(virtualClusterName) {
		return false, nil
	}
	services, err := c.discoverServices(ctx, virtualClusterName, ServiceTypeSQL)
	if err != nil {
		return false, err
	}

	// We only register services for external secondary tenants. If we find any
	// results we know it must be one.
	return len(services) > 0, nil
}

// defaultServiceDescriptors returns the default ports for a service type. This is
// required for scenarios where the services were not registered with a DNS
// provider (Google DNS). Currently, services will not be registered in the
// following scenarios:
//
// 1. A system interface started with default ports. This is an optimisation
// to avoid the overhead of registering services when starting a storage
// cluster with default ports.
// 2. Shared process virtual clusters. These are always started with the same
// ports as the system interface, so we do not register them with DNS.
// 2. Clusters not on GCP
// 3. Clusters that specify a custom project.
//
// Note that because separate process virtual clusters require service registration,
// there is no default port and only shared process services should call this.
func defaultServiceDescriptors(
	virtualClusterName string, nodes Nodes, serviceType ServiceType, sqlInstance int,
) ServiceDescriptors {
	var port int
	switch serviceType {
	case ServiceTypeSQL:
		port = config.DefaultSQLPort
	case ServiceTypeUI:
		port = config.DefaultAdminUIPort
	}
	services := make(ServiceDescriptors, len(nodes))
	for i, node := range nodes {
		services[i] = ServiceDesc{
			VirtualClusterName: virtualClusterName,
			ServiceType:        serviceType,
			ServiceMode:        ServiceModeShared,
			Node:               node,
			Port:               port,
			Instance:           sqlInstance,
		}
	}
	return services
}

// ServiceDescriptors returns the service descriptors for the given nodes and virtual
// cluster. If no services are found, it returns a service descriptor with the default port
// for the service type.
func (c *SyncedCluster) ServiceDescriptors(
	ctx context.Context,
	nodes Nodes,
	virtualClusterName string,
	serviceType ServiceType,
	sqlInstance int,
) (ServiceDescriptors, error) {
	// Not all virtual clusters are registered with DNS, so we must reconstruct our
	// service descriptor based on the registration rules stated in maybeRegisterServices.
	//
	// We first try to discover a service for the virtual cluster name provided on the
	// requested node. If we find a result, we are done.
	services, err := c.discoverServices(
		ctx, virtualClusterName, serviceType,
		ServiceNodePredicate(nodes...), ServiceInstancePredicate(sqlInstance),
	)
	if err != nil {
		return ServiceDescriptors{}, err
	}
	if len(services) > 0 {
		return services, nil
	}

	// If we are looking for the system interface at this point, we know it must be using the default
	// ports, or we would have found it above. Return the default fallback case.
	if IsSystemInterface(virtualClusterName) {
		return defaultServiceDescriptors(
			virtualClusterName, nodes, serviceType, 0, /* sqlInstance */
		), nil
	}

	// If we are looking for a secondary tenant, we know it must be a shared process tenant as all
	// external process services are registered with DNS. Shared process secondary tenants resolve
	// to the system interface, so we must attempt to discover that instead.
	services, err = c.discoverServices(
		ctx, SystemInterfaceName, serviceType, ServiceNodePredicate(nodes...),
	)
	if err != nil {
		return ServiceDescriptors{}, err
	}

	// Update the system service to point to the virtual cluster requested.
	for i := range services {
		services[i].VirtualClusterName = virtualClusterName
		services[i].Instance = sqlInstance
	}

	// If we still have not found a service at this point, it must be a shared process secondary
	// tenant where the system interface is on the default ports.
	if len(services) == 0 {
		return defaultServiceDescriptors(
			virtualClusterName, nodes, serviceType, sqlInstance,
		), nil
	}
	return services, err
}

// ServiceDescriptor is a convenience wrapper for ServiceDescriptors that
// returns only a single service.
func (c *SyncedCluster) ServiceDescriptor(
	ctx context.Context,
	node Node,
	virtualClusterName string,
	serviceType ServiceType,
	sqlInstance int,
) (ServiceDesc, error) {
	services, err := c.ServiceDescriptors(ctx, Nodes{node}, virtualClusterName, serviceType, sqlInstance)
	if err != nil {
		return ServiceDesc{}, err
	}
	return services[0], err
}

// Attempts to fetch the version of the cockroach binary on the first node.
// N.B. For mixed-version clusters, it's the user's responsibility to start only the nodes of
// the same version, at a time.
func (c *SyncedCluster) fetchVersion(
	ctx context.Context, l *logger.Logger, startOpts StartOpts,
) (*version.Version, error) {
	node := c.Nodes[0]
	runVersionCmd := SuppressMetamorphicConstantsEnvVar() + " " + cockroachNodeBinary(c, node) + " version --build-tag"

	result, err := c.runCmdOnSingleNode(ctx, l, node, runVersionCmd, defaultCmdOpts("run-cockroach-version"))
	if err != nil {
		return nil, err
	}
	v, err := version.Parse(strings.TrimSpace(result.CombinedOut))
	return &v, err
}

// Start cockroach on the cluster. For non-multitenant deployments or
// SQL instances that are deployed as external services, this will
// start a cockroach process on the nodes. For shared-process
// virtualization, this creates the virtual cluster metadata (if
// necessary) but does not create any new processes.
//
// Starting the first node is special-cased quite a bit, it's used to distribute
// certs, set cluster settings, and initialize the cluster. Also, if we're only
// starting a single node in the cluster and it happens to be the "first" node
// (node 1, as understood by SyncedCluster.Nodes), we use
// `start-single-node` (this was written to provide a short hand to start a
// single node cluster with a replication factor of one).
func (c *SyncedCluster) Start(ctx context.Context, l *logger.Logger, startOpts StartOpts) error {
	if err := startOpts.validate(c.IsLocal(), c.allowServiceRegistration()); err != nil {
		return err
	}

	if c.IsLocal() {
		// We find open ports dynamically in local mode.
		startOpts.SQLPort = 0
		startOpts.AdminUIPort = 0
	}

	if c.allowServiceRegistration() {
		err := c.maybeRegisterServices(ctx, l, startOpts, c.FindOpenPorts)
		if err != nil {
			return err
		}
	} else {
		l.Printf(strings.Join([]string{
			"WARNING: Service registration and custom ports are not supported for this cluster.",
			fmt.Sprintf("Setting ports to default SQL Port: %d, and Admin UI Port: %d.", config.DefaultSQLPort, config.DefaultAdminUIPort),
			"Attempting to start any additional external SQL processes will fail.",
		}, "\n"))
		startOpts.SQLPort = config.DefaultSQLPort
		startOpts.AdminUIPort = config.DefaultAdminUIPort
	}

	for _, hook := range startOpts.PreStartHooks {
		hookCtx, cancel := context.WithTimeout(ctx, hook.Timeout)
		l.Printf("running pre-start hook: %s", hook.Name)
		err := roachprodutil.PanicAsError(hookCtx, l, hook.Fn)
		cancel()
		if err != nil {
			return err
		}
	}

	if startOpts.IsVirtualCluster() {
		var err error
		startOpts.VirtualClusterID, err = c.upsertVirtualClusterMetadata(ctx, l, startOpts)
		if err != nil {
			return err
		}

		l.Printf("virtual cluster ID: %d", startOpts.VirtualClusterID)

		if err := c.distributeTenantCerts(ctx, l, startOpts.StorageCluster, startOpts.VirtualClusterID); err != nil {
			return err
		}
	} else {
		if err := c.distributeCerts(ctx, l); err != nil {
			return err
		}
	}

	// Start cockroach processes and `init` cluster, if necessary.
	if startOpts.Target != StartSharedProcessForVirtualCluster {
		if parsedVersion, err := c.fetchVersion(ctx, l, startOpts); err == nil {
			// store the version for later checks
			startOpts.Version = parsedVersion
		} else {
			l.Printf("WARN: unable to fetch cockroach version: %s", err)
		}

		l.Printf("%s (%s): starting cockroach processes", c.Name, startOpts.VirtualClusterName)
		if startOpts.IsRestart {
			return c.Parallel(ctx, l, WithNodes(c.Nodes).WithDisplay("starting nodes"), func(ctx context.Context, node Node) (*RunResultDetails, error) {
				return c.startNodeWithResult(ctx, l, node, &startOpts)
			})
		}

		// For single node non-virtual clusters, `init` can be skipped
		// because during the c.StartNode call above, the
		// `--start-single-node` flag will handle all of this for us.
		shouldInit := startOpts.Target == StartDefault && !c.useStartSingleNode() && !startOpts.SkipInit

		for _, node := range c.Nodes {
			// NB: if cockroach started successfully, we ignore the output as it is
			// some harmless start messaging.
			if err := c.startNode(ctx, l, node, &startOpts); err != nil {
				return err
			}
			// We reserve a few special operations (bootstrapping, and setting
			// cluster settings) to the InitTarget.
			if startOpts.GetInitTarget() != node {
				continue
			}

			if shouldInit {
				if err := c.initializeCluster(ctx, l, node); err != nil {
					return err
				}
			}
		}
	}

	// If we did not skip calling `init` on the cluster, we also set up
	// default cluster settings, an admin user, and a backup schedule on
	// the new cluster, making it the cluster a little more realistic
	// and convenient to manage.
	if !startOpts.SkipInit {
		storageCluster := c
		if startOpts.StorageCluster != nil {
			storageCluster = startOpts.StorageCluster
		}

		// We use the `storageCluster` even if starting a virtual cluster
		// because `setClusterSettings` uses SQL statements that are meant
		// to run on the system tenant.
		if err := storageCluster.setClusterSettings(
			ctx, l, startOpts.GetInitTarget(), startOpts.VirtualClusterName,
		); err != nil {
			return err
		}

		c.createAdminUserForSecureCluster(ctx, l, startOpts.VirtualClusterName, startOpts.SQLInstance)

		if startOpts.ScheduleBackups {
			if err := c.createFixedBackupSchedule(ctx, l, startOpts); err != nil {
				return err
			}
		}
	}

	return nil
}

// NodeDir returns the data directory for the given node and store.
func (c *SyncedCluster) NodeDir(node Node, storeIndex int) string {
	if c.IsLocal() {
		if storeIndex != 1 {
			panic("NodeDir only supports one store for local deployments")
		}
		return filepath.Join(c.localVMDir(node), "data")
	}
	return fmt.Sprintf("/mnt/data%d/cockroach", storeIndex)
}

// LogDir returns the logs directory for the given node.
func (c *SyncedCluster) LogDir(node Node, virtualClusterName string, instance int) string {
	dirName := "logs" + virtualClusterDirSuffix(virtualClusterName, instance)
	if c.IsLocal() {
		return filepath.Join(c.localVMDir(node), dirName)
	}
	return dirName
}

// InstanceStoreDir returns the data directory for the given instance of
// the given virtual cluster.
func (c *SyncedCluster) InstanceStoreDir(
	node Node, virtualClusterName string, instance int,
) string {
	dataDir := fmt.Sprintf("data%s", virtualClusterDirSuffix(virtualClusterName, instance))
	if c.IsLocal() {
		return filepath.Join(c.localVMDir(node), dataDir)
	}

	return dataDir
}

// virtualClusterDirSuffix returns the suffix to use for directories specific
// to one virtual cluster.
// This suffix consists of the tenant name and instance number (if applicable).
func virtualClusterDirSuffix(virtualClusterName string, instance int) string {
	if virtualClusterName != "" && virtualClusterName != SystemInterfaceName {
		return fmt.Sprintf("-%s-%d", virtualClusterName, instance)
	}
	return ""
}

// CertsDir returns the certificate directory for the given node.
func (c *SyncedCluster) CertsDir(node Node) string {
	if c.IsLocal() {
		return filepath.Join(c.localVMDir(node), CockroachNodeCertsDir)
	}
	return CockroachNodeCertsDir
}

type PGAuthMode int

const (
	// AuthUserCert authenticates using the default user and password, as well
	// as the root cert and client certs. This uses sslmode=verify-full and will
	// verify that the certificate is valid. This is the preferred mode of
	// authentication.
	AuthUserCert PGAuthMode = iota
	// AuthUserPassword authenticates using the default user and password. Since
	// no certs are specified, sslmode is set to allow. Note this form of auth
	// only works if sslmode=allow is an option, i.e. cockroach sql.
	// AuthUserCert should be used instead most of the time, except when
	// certificates don't exist.
	AuthUserPassword
	// AuthRootCert authenticates using the root user and root cert + root client certs.
	// Root authentication skips verification paths and is not reflective of how real
	// users authenticate.
	AuthRootCert

	DefaultUser     = "roachprod"
	DefaultPassword = "cockroachdb"

	DefaultAuthModeEnv = "ROACHPROD_DEFAULT_AUTH_MODE"
)

// PGAuthModes is a map from auth mode string names to the actual PGAuthMode.
// The string names are used by CLI and the DefaultAuthModeEnv var.
var PGAuthModes = map[string]PGAuthMode{
	"root":          AuthRootCert,
	"user-password": AuthUserPassword,
	"user-cert":     AuthUserCert,
}

// DefaultAuthMode is the auth mode used for functions that don't
// take an auth mode or the user doesn't specify one.
func DefaultAuthMode() PGAuthMode {
	if auth, err := ResolveAuthMode(os.Getenv(DefaultAuthModeEnv)); err == nil {
		return auth
	}
	return AuthUserCert
}

func ResolveAuthMode(authMode string) (PGAuthMode, error) {
	auth, ok := PGAuthModes[authMode]
	if !ok {
		return -1, errors.Newf("unsupported auth-mode %s, valid auth-modes: %v", authMode, maps.Keys(PGAuthModes))
	}
	return auth, nil
}

func (auth PGAuthMode) String() string {
	for modeStr, mode := range PGAuthModes {
		if mode == auth {
			return modeStr
		}
	}

	panic(fmt.Errorf("could not find string for auth mode"))
}

// NodeURL constructs a postgres URL. If virtualClusterName is not empty, it will
// be used as the virtual cluster name in the URL. This is used to connect to a
// shared process running services for multiple virtual clusters.
func (c *SyncedCluster) NodeURL(
	host string,
	port int,
	virtualClusterName string,
	serviceMode ServiceMode,
	auth PGAuthMode,
	database string,
) string {
	var u url.URL
	u.Scheme = "postgres"
	u.User = url.User("root")
	u.Host = fmt.Sprintf("%s:%d", host, port)
	u.Path = database
	v := url.Values{}
	if c.Secure {
		user := DefaultUser
		password := DefaultPassword

		switch auth {
		case AuthRootCert:
			v.Add("sslcert", fmt.Sprintf("%s/client.root.crt", c.PGUrlCertsDir))
			v.Add("sslkey", fmt.Sprintf("%s/client.root.key", c.PGUrlCertsDir))
			v.Add("sslrootcert", fmt.Sprintf("%s/ca.crt", c.PGUrlCertsDir))
			v.Add("sslmode", "verify-full")
		case AuthUserPassword:
			u.User = url.UserPassword(user, password)
			v.Add("sslmode", "allow")
		case AuthUserCert:
			u.User = url.UserPassword(user, password)
			v.Add("sslcert", fmt.Sprintf("%s/client.%s.crt", c.PGUrlCertsDir, user))
			v.Add("sslkey", fmt.Sprintf("%s/client.%s.key", c.PGUrlCertsDir, user))
			v.Add("sslrootcert", fmt.Sprintf("%s/ca.crt", c.PGUrlCertsDir))
			v.Add("sslmode", "verify-full")
		}
	} else {
		v.Add("sslmode", "disable")
	}

	// We only want to pass an explicit `cluster` name if the user provided one.
	if virtualClusterName != "" {
		// We can only pass the cluster parameter for shared processes, as SQL server
		// only processes don't support cluster selection.
		if serviceMode == ServiceModeShared {
			v.Add("options", fmt.Sprintf("-ccluster=%s", virtualClusterName))
		}
	}

	u.RawQuery = v.Encode()
	return "'" + u.String() + "'"
}

// NodePort returns the system tenant's SQL port for the given node.
func (c *SyncedCluster) NodePort(
	ctx context.Context, node Node, virtualClusterName string, sqlInstance int,
) (int, error) {
	desc, err := c.ServiceDescriptor(ctx, node, virtualClusterName, ServiceTypeSQL, sqlInstance)
	if err != nil {
		return 0, err
	}
	return desc.Port, nil
}

// NodeUIPort returns the system tenant's AdminUI port for the given node.
func (c *SyncedCluster) NodeUIPort(
	ctx context.Context, node Node, virtualClusterName string, sqlInstance int,
) (int, error) {
	desc, err := c.ServiceDescriptor(ctx, node, virtualClusterName, ServiceTypeUI, sqlInstance)
	if err != nil {
		return 0, err
	}
	return desc.Port, nil
}

// ExecOrInteractiveSQL ssh's onto a single node and executes `./ cockroach sql`
// with the provided args, potentially opening an interactive session. Note
// that the caller can pass the `--e` flag to execute sql cmds and exit the
// session. See `./cockroch sql -h` for more options.
//
// CAUTION: this function should not be used by roachtest writers. Use ExecSQL below.
func (c *SyncedCluster) ExecOrInteractiveSQL(
	ctx context.Context,
	l *logger.Logger,
	virtualClusterName string,
	sqlInstance int,
	authMode PGAuthMode,
	database string,
	args []string,
) error {
	if len(c.Nodes) != 1 {
		return fmt.Errorf("invalid number of nodes for interactive sql: %d", len(c.Nodes))
	}
	desc, err := c.ServiceDescriptor(ctx, c.Nodes[0], virtualClusterName, ServiceTypeSQL, sqlInstance)
	if err != nil {
		return err
	}
	url := c.NodeURL("localhost", desc.Port, virtualClusterName, desc.ServiceMode, authMode, database)
	binary := cockroachNodeBinary(c, c.Nodes[0])
	allArgs := []string{binary, "sql", "--url", url}
	allArgs = append(allArgs, ssh.Escape(args))
	return c.SSH(ctx, l, []string{"-t"}, allArgs)
}

// ExecSQL runs a `cockroach sql` and returns the output.  If the call
// is intended to run a SQL statement, the caller must pass the "-e"
// (or "--execute") flag explicitly.
func (c *SyncedCluster) ExecSQL(
	ctx context.Context,
	l *logger.Logger,
	nodes Nodes,
	virtualClusterName string,
	sqlInstance int,
	authMode PGAuthMode,
	database string,
	args []string,
) ([]*RunResultDetails, error) {
	display := fmt.Sprintf("%s: executing sql", c.Name)
	results, _, err := c.ParallelE(ctx, l, WithNodes(nodes).WithDisplay(display).WithFailSlow(),
		func(ctx context.Context, node Node) (*RunResultDetails, error) {
			desc, err := c.ServiceDescriptor(ctx, node, virtualClusterName, ServiceTypeSQL, sqlInstance)
			if err != nil {
				return nil, err
			}
			var cmd string
			if c.IsLocal() {
				cmd = fmt.Sprintf(`cd %s ; `, c.localVMDir(node))
			}
			cmd += SuppressMetamorphicConstantsEnvVar() + " " + cockroachNodeBinary(c, node) + " sql --url " +
				c.NodeURL("localhost", desc.Port, virtualClusterName, desc.ServiceMode, authMode, database) + " " +
				ssh.Escape(args)
			return c.runCmdOnSingleNode(ctx, l, node, cmd, defaultCmdOpts("run-sql"))
		})

	return results, err
}

// N.B. not thread-safe because startOpts is shared and may be mutated.
func (c *SyncedCluster) startNodeWithResult(
	ctx context.Context, l *logger.Logger, node Node, startOpts *StartOpts,
) (*RunResultDetails, error) {
	l.Printf("starting node %d", node)
	startScriptPath := StartScriptPath(startOpts.VirtualClusterName, startOpts.SQLInstance)
	var runScriptCmd string
	if c.IsLocal() {
		runScriptCmd = fmt.Sprintf(`cd %s ; `, c.localVMDir(node))
	}
	runScriptCmd += "./" + startScriptPath

	// If we are performing a restart, the start script should already
	// exist, and we are going to reuse it.
	if !startOpts.IsRestart {
		startCmd, err := c.generateStartCmd(ctx, l, node, startOpts)
		if err != nil {
			return newRunResultDetails(node, err), err
		}
		var uploadCmd string
		if c.IsLocal() {
			uploadCmd = fmt.Sprintf(`cd %s ; `, c.localVMDir(node))
		}
		uploadCmd += fmt.Sprintf(`cat > %[1]s && chmod +x %[1]s`, startScriptPath)

		var res = &RunResultDetails{}
		uploadOpts := defaultCmdOpts("upload-start-script")
		uploadOpts.stdin = strings.NewReader(startCmd)
		res, err = c.runCmdOnSingleNode(ctx, l, node, uploadCmd, uploadOpts)
		if err != nil || res.Err != nil {
			return res, err
		}
	}

	return c.runCmdOnSingleNode(ctx, l, node, runScriptCmd, defaultCmdOpts("run-start-script"))
}

// N.B. not thread-safe because startOpts is shared and may be mutated.
func (c *SyncedCluster) startNode(
	ctx context.Context, l *logger.Logger, node Node, startOpts *StartOpts,
) error {
	res, err := c.startNodeWithResult(ctx, l, node, startOpts)
	return errors.CombineErrors(err, res.Err)
}

func (c *SyncedCluster) generateStartCmd(
	ctx context.Context, l *logger.Logger, node Node, startOpts *StartOpts,
) (string, error) {
	args, err := c.generateStartArgs(ctx, l, node, startOpts)
	if err != nil {
		return "", err
	}
	keyCmd, err := c.generateKeyCmd(ctx, l, node, startOpts)
	if err != nil {
		return "", err
	}

	return execStartTemplate(startTemplateData{
		LogDir: c.LogDir(node, startOpts.VirtualClusterName, startOpts.SQLInstance),
		KeyCmd: keyCmd,
		EnvVars: append(append([]string{
			fmt.Sprintf("ROACHPROD=%s", c.roachprodEnvValue(node)),
			"GOTRACEBACK=crash",
			// N.B. disable telemetry (see `TelemetryOptOut`).
			"COCKROACH_SKIP_ENABLING_DIAGNOSTIC_REPORTING=1",
			// N.B. set crash reporting URL (see `crashReportURL()`) to the empty string to disable Sentry crash reports.
			"COCKROACH_CRASH_REPORTS=",
		}, c.Env...), getEnvVars()...),
		Binary:              cockroachNodeBinary(c, node),
		Args:                args,
		MemoryMax:           config.MemoryMax,
		NumFilesLimit:       startOpts.NumFilesLimit,
		VirtualClusterLabel: VirtualClusterLabel(startOpts.VirtualClusterName, startOpts.SQLInstance),
		Local:               c.IsLocal(),
	})
}

type startTemplateData struct {
	Local               bool
	LogDir              string
	Binary              string
	KeyCmd              string
	MemoryMax           string
	NumFilesLimit       int64
	VirtualClusterLabel string
	Args                []string
	EnvVars             []string
}

type loggingTemplateData struct {
	LogDir string
}

// VirtualClusterLabel is the value used to "label" virtual cluster
// (cockroach) processes running locally or in a VM. This is used by
// roachprod to monitor identify such processes and monitor them.
func VirtualClusterLabel(virtualClusterName string, sqlInstance int) string {
	if virtualClusterName == "" || virtualClusterName == SystemInterfaceName {
		return "cockroach-system"
	}

	return fmt.Sprintf("cockroach-%s_%d", virtualClusterName, sqlInstance)
}

// VirtualClusterInfoFromLabel takes as parameter a tenant label
// produced with `VirtuaLClusterLabel()` and returns the corresponding
// tenant name and instance.
func VirtualClusterInfoFromLabel(virtualClusterLabel string) (string, int, error) {
	var (
		sqlInstance          int
		sqlInstanceStr       string
		labelWithoutInstance string
		err                  error
	)

	sep := "_"
	parts := strings.Split(virtualClusterLabel, sep)

	// Note that this logic assumes that virtual cluster names cannot
	// have a '_' character, which is currently (Sep 2023) the case.
	switch len(parts) {
	case 1:
		// This should be a system tenant (no instance identifier)
		labelWithoutInstance = parts[0]

	case 2:
		// SQL instance process: instance number is after the '_' character.
		labelWithoutInstance, sqlInstanceStr = parts[0], parts[1]
		sqlInstance, err = strconv.Atoi(sqlInstanceStr)
		if err != nil {
			return "", 0, fmt.Errorf("invalid virtual cluster label: %s", virtualClusterLabel)
		}

	default:
		return "", 0, fmt.Errorf("invalid virtual cluster label: %s", virtualClusterLabel)
	}

	// Remove the "cockroach-" prefix added by VirtualClusterLabel.
	virtualClusterName := strings.TrimPrefix(labelWithoutInstance, "cockroach-")
	return virtualClusterName, sqlInstance, nil
}

func StartScriptPath(virtualClusterName string, sqlInstance int) string {
	// Use the default start script name for the system tenant (storage cluster).
	if virtualClusterName == SystemInterfaceName || virtualClusterName == "" {
		return "cockroach.sh"
	}
	return fmt.Sprintf("cockroach-%s.sh", VirtualClusterLabel(virtualClusterName, sqlInstance))
}

func execStartTemplate(data startTemplateData) (string, error) {
	tpl, err := template.New("start").
		Funcs(template.FuncMap{"shesc": func(i interface{}) string {
			return shellescape.Quote(fmt.Sprint(i))
		}}).
		Delims("#{", "#}").
		Parse(startScript)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := tpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func execLoggingTemplate(data loggingTemplateData) (string, error) {
	tpl, err := template.New("loggingConfig").
		Delims("#{", "#}").
		Parse(loggingConfig)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := tpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// generateStartArgs generates cockroach binary arguments for starting a node.
// The first argument is the command (e.g. "start").
func (c *SyncedCluster) generateStartArgs(
	ctx context.Context, l *logger.Logger, node Node, startOpts *StartOpts,
) ([]string, error) {
	var args []string

	switch startOpts.Target {
	case StartDefault:
		if c.useStartSingleNode() {
			args = []string{"start-single-node"}
		} else {
			args = []string{"start"}
		}

	case StartServiceForVirtualCluster:
		args = []string{"mt", "start-sql"}

	default:
		return nil, errors.Errorf("unsupported start target %v", startOpts.Target)
	}

	// Flags common to all targets.

	if c.Secure {
		args = append(args, `--certs-dir`, c.CertsDir(node))
	} else {
		args = append(args, "--insecure")
	}

	logDir := c.LogDir(node, startOpts.VirtualClusterName, startOpts.SQLInstance)
	idx1 := argExists(startOpts.ExtraArgs, "--log")
	idx2 := argExists(startOpts.ExtraArgs, "--log-config-file")

	// if neither --log nor --log-config-file are present
	if idx1 == -1 && idx2 == -1 {
		if !startOpts.EnableFluentSink {
			args = append(args, "--log", `file-defaults: {dir: '`+logDir+`', exit-on-error: false}`)
		} else {
			loggingConfig, err := execLoggingTemplate(loggingTemplateData{
				LogDir: logDir,
			})
			if err != nil {
				return nil, errors.Wrap(err, "failed rendering logging template")
			}

			loggingConfigFile := fmt.Sprintf("cockroachdb-logging%s.yaml",
				virtualClusterDirSuffix(startOpts.VirtualClusterName, startOpts.SQLInstance))

			// To speed up the startup time of nodes in large cluster, the cockroachdb-logging.yaml file is copied
			// to all nodes in parallel.
			if c.Nodes[0] == node {
				if err := c.PutString(ctx, l, c.Nodes, loggingConfig, loggingConfigFile, 0644); err != nil {
					return nil, errors.Wrap(err, "failed writing remote logging configuration: %w")
				}
			}

			args = append(args, "--log-config-file", loggingConfigFile)
		}
	}

	listenHost := ""
	if c.IsLocal() && runtime.GOOS == "darwin " {
		// This avoids annoying firewall prompts on Mac OS X.
		listenHost = "127.0.0.1"
	}

	virtualClusterName := startOpts.VirtualClusterName
	instance := startOpts.SQLInstance
	var sqlPort int
	if startOpts.Target == StartServiceForVirtualCluster {
		desc, err := c.ServiceDescriptor(ctx, node, virtualClusterName, ServiceTypeSQL, instance)
		if err != nil {
			return nil, err
		}
		sqlPort = desc.Port
		args = append(args, fmt.Sprintf("--sql-addr=%s:%d", listenHost, sqlPort))
	} else {
		virtualClusterName = SystemInterfaceName
		// System interface instance is always 0.
		instance = 0
		desc, err := c.ServiceDescriptor(ctx, node, virtualClusterName, ServiceTypeSQL, instance)
		if err != nil {
			return nil, err
		}
		sqlPort = desc.Port
		args = append(args, fmt.Sprintf("--listen-addr=%s:%d", listenHost, sqlPort))
	}
	desc, err := c.ServiceDescriptor(ctx, node, virtualClusterName, ServiceTypeUI, instance)
	if err != nil {
		return nil, err
	}
	args = append(args, fmt.Sprintf("--http-addr=%s:%d", listenHost, desc.Port))

	if !c.IsLocal() {
		advertiseHost := ""
		if c.shouldAdvertisePublicIP() {
			advertiseHost = c.Host(node)
		} else {
			advertiseHost = c.VMs[node-1].PrivateIP
		}
		args = append(args,
			fmt.Sprintf("--advertise-addr=%s:%d", advertiseHost, sqlPort),
		)
	}

	// --join flags are unsupported/unnecessary in `cockroach start-single-node`.
	if startOpts.Target == StartDefault && !c.useStartSingleNode() {
		joinTargets := startOpts.GetJoinTargets()
		addresses := make([]string, len(joinTargets))
		for i, joinNode := range startOpts.GetJoinTargets() {
			desc, err := c.ServiceDescriptor(ctx, joinNode, SystemInterfaceName, ServiceTypeSQL, 0)
			if err != nil {
				return nil, err
			}
			addresses[i] = fmt.Sprintf("%s:%d", c.Host(joinNode), desc.Port)
		}
		args = append(args, fmt.Sprintf("--join=%s", strings.Join(addresses, ",")))
	}
	if startOpts.Target == StartServiceForVirtualCluster {
		storageAddrs, err := startOpts.StorageCluster.allPublicAddrs(ctx)
		if err != nil {
			return nil, err
		}

		args = append(args, fmt.Sprintf("--kv-addrs=%s", storageAddrs))
		args = append(args, fmt.Sprintf("--tenant-id=%d", startOpts.VirtualClusterID))
	}

	if startOpts.Target == StartDefault {
		args = append(args, c.generateStartFlagsKV(l, node, startOpts)...)
	}

	if startOpts.Target == StartDefault || startOpts.Target == StartServiceForVirtualCluster {
		args = append(args, c.generateStartFlagsSQL(node, startOpts)...)
	}

	args = append(args, startOpts.ExtraArgs...)

	// Argument template expansion is node specific (e.g. for {store-dir}).
	e := expander{
		node: node,
	}
	// We currently don't accept any custom expander configurations in
	// this function.
	var expanderConfig ExpanderConfig
	for i, arg := range args {
		expandedArg, err := e.expand(ctx, l, c, expanderConfig, arg)
		if err != nil {
			return nil, err
		}
		args[i] = expandedArg
	}

	return args, nil
}

// generateStartFlagsKV generates `cockroach start` arguments that are relevant
// for the KV and storage layers (and consequently are never used by
// `cockroach mt start-sql`).
func (c *SyncedCluster) generateStartFlagsKV(
	l *logger.Logger, node Node, startOpts *StartOpts,
) []string {
	var args []string
	var storeDirs []string
	if idx := argExists(startOpts.ExtraArgs, "--store"); idx == -1 {
		for i := 1; i <= startOpts.StoreCount; i++ {
			storeDir := c.NodeDir(node, i)
			storeDirs = append(storeDirs, storeDir)
			// Place a store{i} attribute on each store to allow for zone configs
			// that use specific stores. Note that `i` is 1 most of the time, since
			// it's the i-th store on the *current* node. This isn't always useful,
			// for example it doesn't let one single out a specific node. We add
			// nodeX-flavor attributes for that.
			args = append(args, `--store`,
				fmt.Sprintf(`path=%s,attrs=store%d:node%d:node%dstore%d`, storeDir, i, node, node, i))
		}
	} else if strings.HasPrefix(startOpts.ExtraArgs[idx], "--store=") {
		// The flag and path were provided together. Strip the flag prefix.
		storeDir := strings.TrimPrefix(startOpts.ExtraArgs[idx], "--store=")
		if !strings.Contains(storeDir, "type=mem") {
			storeDirs = append(storeDirs, storeDir)
		}
	} else {
		// Else, the store flag and path were specified as separate arguments. The
		// path is the subsequent arg.
		storeDirs = append(storeDirs, startOpts.ExtraArgs[idx+1])
	}

	if startOpts.EncryptedStores {
		// Encryption at rest is turned on for the cluster.
		for _, storeDir := range storeDirs {
			// TODO(windchan7): allow key size to be specified through flags.
			encryptArgs := "path=%s,key=%s/aes-128.key,old-key=plain"
			encryptArgs = fmt.Sprintf(encryptArgs, storeDir, storeDir)
			args = append(args, `--enterprise-encryption`, encryptArgs)
		}
	}
	if startOpts.WALFailover != "" {
		// N.B. WALFailover is only supported in v24+.
		// If version is unknown, we only set WALFailover if StoreCount > 1.
		// To silence redundant warnings, when other nodes are started, we reset WALFailover.
		if startOpts.Version != nil && startOpts.Version.Major().Year < 24 {
			l.Printf("WARN: WALFailover is only supported in v24+. Ignoring --wal-failover flag.")
			startOpts.WALFailover = ""
		} else if startOpts.Version == nil && startOpts.StoreCount <= 1 {
			l.Printf("WARN: StoreCount <= 1; ignoring --wal-failover flag.")
			startOpts.WALFailover = ""
		} else {
			args = append(args, fmt.Sprintf("--wal-failover=%s", startOpts.WALFailover))
		}
	}

	args = append(args, fmt.Sprintf("--cache=%d%%", c.maybeScaleMem(25)))

	if localityArg := c.generateLocalityArg(node, startOpts); localityArg != "" {
		args = append(args, localityArg)
	}
	return args
}

var maxSQLMemoryRE = regexp.MustCompile(`^--max-sql-memory=(\d+)%$`)

// generateStartFlagsSQL generates `cockroach start` and `cockroach mt
// start-sql` arguments that are relevant for the SQL layers, used by both KV
// and storage layers.
func (c *SyncedCluster) generateStartFlagsSQL(node Node, startOpts *StartOpts) []string {
	var args []string
	formatArg := func(m int) string {
		return fmt.Sprintf("--max-sql-memory=%d%%", c.maybeScaleMem(m))
	}

	if idx := argExists(startOpts.ExtraArgs, "--max-sql-memory"); idx == -1 {
		args = append(args, formatArg(25))
	} else {
		arg := startOpts.ExtraArgs[idx]
		matches := maxSQLMemoryRE.FindStringSubmatch(arg)
		mem, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil {
			panic(fmt.Sprintf("invalid --max-sql-memory parameter: %s", arg))
		}

		startOpts.ExtraArgs[idx] = formatArg(int(mem))
	}

	if startOpts.Target == StartServiceForVirtualCluster {
		args = append(args, "--store", c.InstanceStoreDir(node, startOpts.VirtualClusterName, startOpts.SQLInstance))

		if localityArg := c.generateLocalityArg(node, startOpts); localityArg != "" {
			args = append(args, localityArg)
		}
	}
	return args
}

func (c *SyncedCluster) generateLocalityArg(node Node, startOpts *StartOpts) string {
	if locality := c.locality(node); locality != "" {
		if idx := argExists(startOpts.ExtraArgs, "--locality"); idx == -1 {
			return "--locality=" + locality
		}
	}

	return ""
}

// maybeScaleMem is used to scale down a memory percentage when the cluster is
// local.
func (c *SyncedCluster) maybeScaleMem(val int) int {
	if c.IsLocal() {
		val /= len(c.Nodes)
		if val == 0 {
			val = 1
		}
	}
	return val
}

func (c *SyncedCluster) initializeCluster(ctx context.Context, l *logger.Logger, node Node) error {
	l.Printf("%s: initializing cluster\n", c.Name)
	cmd, err := c.generateInitCmd(ctx, node)
	if err != nil {
		return err
	}

	res, err := c.runCmdOnSingleNode(ctx, l, node, cmd, defaultCmdOpts("init-cluster"))
	if res != nil {
		if out := strings.TrimSpace(res.CombinedOut); out != "" {
			l.Printf(out)
		}
	}
	return errors.CombineErrors(err, res.Err)
}

// createAdminUserForSecureCluster creates a `roach` user with admin
// privileges. The password used matches the virtual cluster name
// ('system' for the storage cluster). If it cannot be created, this
// function will log an error and continue. Roachprod is used in a
// variety of contexts within roachtests, and a failure to create a
// user might be "expected" depending on what the test is doing.
func (c *SyncedCluster) createAdminUserForSecureCluster(
	ctx context.Context, l *logger.Logger, virtualClusterName string, sqlInstance int,
) {
	if !c.Secure {
		return
	}

	// Use the same combination for username and password to allow
	// people using the DB console to easily switch between tenants in
	// the UI, if managing UA clusters.
	const username = DefaultUser
	const password = DefaultPassword

	stmts := strings.Join([]string{
		fmt.Sprintf("CREATE USER IF NOT EXISTS %s WITH LOGIN PASSWORD '%s'", username, password),
		fmt.Sprintf("GRANT ADMIN TO %s WITH ADMIN OPTION", username),
	}, "; ")

	// We retry a few times here because cockroach process might not be
	// ready to serve connections at this point. We also can't use
	// `COCKROACH_CONNECT_TIMEOUT` in this case because that would not
	// work for shared-process virtual clusters.
	retryOpts := retry.Options{MaxRetries: 20}
	if err := retryOpts.Do(ctx, func(ctx context.Context) error {
		// We use the first node in the virtual cluster to create the user.
		firstNode := c.Nodes[0]
		results, err := c.ExecSQL(
			ctx, l, Nodes{firstNode}, virtualClusterName, sqlInstance, AuthRootCert, "", /* database */
			[]string{"-e", stmts})

		if err != nil || results[0].Err != nil {
			err := errors.CombineErrors(err, results[0].Err)
			return err
		}

		return nil
	}); err != nil {
		l.Printf("not creating default admin user due to error: %v", err)
		return
	}

	var virtualClusterInfo string
	if virtualClusterName != SystemInterfaceName {
		virtualClusterInfo = fmt.Sprintf(" for virtual cluster %s", virtualClusterName)
	}

	l.Printf("log into DB console%s with user=%s password=%s", virtualClusterInfo, DefaultUser, password)
}

func (c *SyncedCluster) setClusterSettings(
	ctx context.Context, l *logger.Logger, node Node, virtualCluster string,
) error {
	l.Printf("%s (%s): setting cluster settings", c.Name, virtualCluster)
	cmd, err := c.generateClusterSettingCmd(ctx, l, node, virtualCluster)
	if err != nil {
		return err
	}

	res, err := c.runCmdOnSingleNode(ctx, l, node, cmd, defaultCmdOpts("set-cluster-settings"))
	if res != nil {
		out := strings.TrimSpace(res.CombinedOut)
		if out != "" {
			l.Printf(out)
		}
	}

	if res != nil && res.Err != nil {
		err = errors.CombineErrors(err, res.Err)
	}

	return err
}

// Use env.COCKROACH_DEV_LICENSE when valid; otherwise, generate a fresh one.
func (c *SyncedCluster) maybeGenerateLicense(l *logger.Logger) string {
	var res string

	if config.CockroachDevLicense != "" {
		license, err := licenseccl.Decode(config.CockroachDevLicense)
		if err != nil {
			l.Printf("WARN: (cluster=%q) failed to decode COCKROACH_DEV_LICENSE: %s", c.Name, err)
		} else if license.ValidUntilUnixSec < timeutil.Now().AddDate(0, 0, 1).Unix() {
			l.Printf("WARN: (cluster=%q) COCKROACH_DEV_LICENSE has effectively expired: %s", c.Name,
				timeutil.Unix(license.ValidUntilUnixSec, 0).Format(time.RFC3339))
		} else {
			res = config.CockroachDevLicense
			l.Printf("INFO: (cluster=%q) using COCKROACH_DEV_LICENSE: %v", c.Name, license)
		}
	}
	if res == "" {
		res, _ = (&licenseccl.License{
			Type: licenseccl.License_Enterprise,
			// OrganizationName needs to be set to preserve backwards compatibility.
			OrganizationName:  "Cockroach Labs - Production Testing",
			Environment:       licenseccl.Development,
			ValidUntilUnixSec: timeutil.Now().AddDate(0, 1, 0).Unix(),
		}).Encode()
		l.Printf("(cluster=%q) generated a fresh license: %v ", c.Name, res)
	}
	return res
}

func (c *SyncedCluster) generateClusterSettingCmd(
	ctx context.Context, l *logger.Logger, node Node, virtualCluster string,
) (string, error) {
	license := c.maybeGenerateLicense(l)
	var tenantPrefix string
	if virtualCluster != "" && virtualCluster != SystemInterfaceName {
		tenantPrefix = fmt.Sprintf("ALTER TENANT '%s' ", virtualCluster)
	}

	clusterSettings := map[string]string{
		"cluster.organization": "Cockroach Labs - Production Testing",
		"enterprise.license":   license,
		// N.B. We now enable `PanicOnAssertions` for all roachprod clusters.
		// (See https://github.com/cockroachdb/cockroach/issues/136858)
		// Use the internal name instead of the user visible name, which wasn't
		// added until 23.2, to avoid breaking mixed version tests.
		"debug.panic_on_failed_assertions": "true",
	}
	for name, value := range c.ClusterSettings.ClusterSettings {
		clusterSettings[name] = value
	}
	var clusterSettingsString string
	for name, value := range clusterSettings {
		clusterSettingsString += fmt.Sprintf("%sSET CLUSTER SETTING %s = '%s';\n", tenantPrefix, name, value)
	}

	var clusterSettingsCmd string
	if c.IsLocal() {
		clusterSettingsCmd = fmt.Sprintf(`cd %s ; `, c.localVMDir(node))
	}

	binary := cockroachNodeBinary(c, node)
	var pathPrefix string
	if virtualCluster != "" {
		pathPrefix = fmt.Sprintf("%s_", virtualCluster)
	}
	path := fmt.Sprintf("%s/%ssettings-initialized", c.NodeDir(node, 1 /* storeIndex */), pathPrefix)
	port, err := c.NodePort(ctx, node, "" /* virtualClusterName */, 0 /* sqlInstance */)
	if err != nil {
		return "", err
	}
	url := c.NodeURL("localhost", port, SystemInterfaceName /* virtualClusterName */, ServiceModeShared, AuthRootCert, "" /* database */)

	// We use `mkdir -p` here since the directory may not exist if an in-memory
	// store is used.
	clusterSettingsCmd += fmt.Sprintf(`
		if ! test -e %s ; then
			%s COCKROACH_CONNECT_TIMEOUT=%d %s sql --url %s -e "%s" && mkdir -p %s && touch %s
		fi`, path, SuppressMetamorphicConstantsEnvVar(), startSQLTimeout, binary, url, clusterSettingsString, c.NodeDir(node, 1 /* storeIndex */), path)
	return clusterSettingsCmd, nil
}

func (c *SyncedCluster) generateInitCmd(ctx context.Context, node Node) (string, error) {
	var initCmd string
	if c.IsLocal() {
		initCmd = fmt.Sprintf(`cd %s ; `, c.localVMDir(node))
	}

	path := fmt.Sprintf("%s/%s", c.NodeDir(node, 1 /* storeIndex */), "cluster-bootstrapped")
	port, err := c.NodePort(ctx, node, "" /* virtualClusterName */, 0 /* sqlInstance */)
	if err != nil {
		return "", err
	}
	url := c.NodeURL("localhost", port, SystemInterfaceName /* virtualClusterName */, ServiceModeShared, AuthRootCert, "" /* database */)
	binary := cockroachNodeBinary(c, node)
	initCmd += fmt.Sprintf(`
		if ! test -e %[1]s ; then
			%[2]s COCKROACH_CONNECT_TIMEOUT=%[5]d %[3]s init --url %[4]s && touch %[1]s
		fi`, path, SuppressMetamorphicConstantsEnvVar(), binary, url, startSQLTimeout)
	return initCmd, nil
}

func (c *SyncedCluster) generateKeyCmd(
	ctx context.Context, l *logger.Logger, node Node, startOpts *StartOpts,
) (string, error) {
	if !startOpts.EncryptedStores {
		return "", nil
	}

	var storeDirs []string
	if storeArgIdx := argExists(startOpts.ExtraArgs, "--store"); storeArgIdx == -1 {
		for i := 1; i <= startOpts.StoreCount; i++ {
			storeDir := c.NodeDir(node, i)
			storeDirs = append(storeDirs, storeDir)
		}
	} else if startOpts.ExtraArgs[storeArgIdx] == "--store=" {
		// The flag and path were provided together. Strip the flag prefix.
		storeDir := strings.TrimPrefix(startOpts.ExtraArgs[storeArgIdx], "--store=")
		storeDirs = append(storeDirs, storeDir)
	} else {
		// Else, the store flag and path were specified as separate arguments. The
		// path is the subsequent arg.
		storeDirs = append(storeDirs, startOpts.ExtraArgs[storeArgIdx+1])
	}

	// Command to create the store key.
	var keyCmd strings.Builder
	for _, storeDir := range storeDirs {
		fmt.Fprintf(&keyCmd, `
			mkdir -p %[1]s;
			if [ ! -e %[1]s/aes-128.key ]; then
				openssl rand -out %[1]s/aes-128.key 48;
			fi;`, storeDir)
	}

	e := expander{node: node}
	// We currently don't accept any custom expander configurations in
	// this function.
	var expanderConfig ExpanderConfig
	expanded, err := e.expand(ctx, l, c, expanderConfig, keyCmd.String())
	if err != nil {
		return "", err
	}
	return expanded, nil
}

func (c *SyncedCluster) useStartSingleNode() bool {
	return len(c.VMs) == 1
}

// distributeCerts distributes certs if it's a secure cluster and we're
// starting n1.
func (c *SyncedCluster) distributeCerts(ctx context.Context, l *logger.Logger) error {
	if !c.Secure {
		return nil
	}
	for _, node := range c.Nodes {
		if node == 1 {
			return c.DistributeCerts(ctx, l, false)
		}
	}
	return nil
}

// upsertVirtualClusterMetadata creates the virtual cluster metadata,
// if necessary, and marks the service as started internally. We only
// need to run the statements in this function against a single
// connection to the storage cluster.
func (c *SyncedCluster) upsertVirtualClusterMetadata(
	ctx context.Context, l *logger.Logger, startOpts StartOpts,
) (int, error) {
	runSQL := func(stmt string) (string, error) {
		var results []*RunResultDetails
		var err error
		// It is possible to target a storage node that is currently down, so
		// we should attempt connecting to all storage nodes before erroring out.
		for n := 0; n < len(startOpts.StorageCluster.Nodes); n++ {
			results, err = startOpts.StorageCluster.ExecSQL(
				ctx, l, startOpts.StorageCluster.Nodes[n:n+1], SystemInterfaceName, 0, DefaultAuthMode(), "", /* database */
				[]string{"--format", "csv", "-e", stmt})
			if err == nil && results[0].Err == nil {
				return results[0].CombinedOut, nil
			}
			if err != nil {
				l.Printf("failed to execute SQL statement %q on node %d: %s", stmt, n, err)
			} else if results[0].Err != nil {
				l.Printf("failed to execute SQL statement %q on node %d: %s", stmt, n, err)
			}
		}
		if err != nil {
			return "", err
		}
		if results[0].Err != nil {
			return "", results[0].Err
		}

		return results[0].CombinedOut, nil
	}

	virtualClusterIDByName := func(name string) (int, error) {
		query := fmt.Sprintf(
			"SELECT id FROM system.tenants WHERE name = '%s'", startOpts.VirtualClusterName,
		)

		existsOut, err := runSQL(query)
		if err != nil {
			return -1, err
		}

		rows, err := csv.NewReader(strings.NewReader(existsOut)).ReadAll()
		if err != nil {
			return -1, fmt.Errorf("failed to parse system.tenants output: %w\n%s", err, existsOut)
		}

		records := rows[1:] // skip header
		if len(records) == 0 {
			return -1, nil
		}

		n, err := strconv.Atoi(records[0][0])
		if err != nil {
			return -1, fmt.Errorf("failed to parse virtual cluster ID: %w", err)
		}

		return n, nil
	}

	virtualClusterID, err := virtualClusterIDByName(startOpts.VirtualClusterName)
	if err != nil {
		return -1, err
	}

	l.Printf("Starting virtual cluster")
	serviceMode := "SHARED"
	if startOpts.Target == StartServiceForVirtualCluster {
		serviceMode = "EXTERNAL"
	}

	if virtualClusterID <= 0 {
		// If the virtual cluster metadata does not exist yet, create it.
		_, err = runSQL(fmt.Sprintf("CREATE TENANT '%s'", startOpts.VirtualClusterName))
		if err != nil {
			return -1, err
		}
	}

	_, err = runSQL(fmt.Sprintf("ALTER TENANT '%s' START SERVICE %s", startOpts.VirtualClusterName, serviceMode))
	if err != nil {
		return -1, err
	}

	return virtualClusterIDByName(startOpts.VirtualClusterName)
}

// distributeCerts distributes certs if it's a secure cluster.
func (c *SyncedCluster) distributeTenantCerts(
	ctx context.Context, l *logger.Logger, storageCluster *SyncedCluster, virtualClusterID int,
) error {
	if c.Secure {
		return c.DistributeTenantCerts(ctx, l, storageCluster, virtualClusterID)
	}
	return nil
}

func (c *SyncedCluster) shouldAdvertisePublicIP() bool {
	// If we're creating nodes that span VPC (e.g. AWS multi-region or
	// multi-cloud), we'll tell the nodes to advertise their public IPs
	// so that attaching nodes to the cluster Just Works.
	for i := range c.VMs {
		if i > 0 && c.VMs[i].VPC != c.VMs[0].VPC {
			return true
		}
	}
	return false
}

// createFixedBackupSchedule creates a cluster backup schedule which, by
// default, runs an incremental every 15 minutes and a full every hour. On
// `roachprod create`, the user can provide a different recurrence using the
// 'schedule-backup-args' flag. If roachprod is local, the backups get stored in
// nodelocal, and otherwise in 'gs://cockroachdb-backup-testing'.
// This cmd also ensures that only one schedule will be created for the cluster.
func (c *SyncedCluster) createFixedBackupSchedule(
	ctx context.Context, l *logger.Logger, startOpts StartOpts,
) error {
	externalStoragePath := fmt.Sprintf("gs://%s", testutils.BackupTestingBucket())
	for _, cloud := range c.Clouds() {
		if !strings.Contains(cloud, gce.ProviderName) {
			l.Printf(`no scheduled backup created as there exists a vm not on google cloud`)
			return nil
		}
	}
	l.Printf("%s (%s): creating backup schedule", c.Name, startOpts.VirtualClusterName)
	auth := "AUTH=implicit"
	collectionPath := fmt.Sprintf(`%s/roachprod-scheduled-backups/%s/%s/%v?%s`,
		externalStoragePath, c.Name, startOpts.VirtualClusterName, timeutil.Now().UnixNano(), auth)

	createScheduleCmd := fmt.Sprintf(`CREATE SCHEDULE IF NOT EXISTS test_only_backup FOR BACKUP INTO '%s' %s`,
		collectionPath, startOpts.ScheduleBackupArgs)

	node := c.Nodes[0]
	binary := cockroachNodeBinary(c, node)
	port, err := c.NodePort(ctx, node, startOpts.VirtualClusterName, startOpts.SQLInstance)
	if err != nil {
		return err
	}
	serviceMode := ServiceModeShared
	if startOpts.Target == StartServiceForVirtualCluster {
		serviceMode = ServiceModeExternal
	}

	url := c.NodeURL("localhost", port, startOpts.VirtualClusterName, serviceMode, AuthRootCert, "" /* database */)
	fullCmd := fmt.Sprintf(`%s COCKROACH_CONNECT_TIMEOUT=%d %s sql --url %s -e %q`,
		SuppressMetamorphicConstantsEnvVar(), startSQLTimeout, binary, url, createScheduleCmd)
	// Instead of using `c.ExecSQL()`, use `c.runCmdOnSingleNode()`, which allows us to
	// 1) prefix the schedule backup cmd with COCKROACH_CONNECT_TIMEOUT.
	// 2) run the command against the first node in the cluster target.
	res, err := c.runCmdOnSingleNode(ctx, l, node, fullCmd, defaultCmdOpts("init-backup-schedule"))
	if err != nil || res.Err != nil {
		out := ""
		if res != nil {
			out = res.CombinedOut
		}
		return errors.Wrapf(errors.CombineErrors(err, res.Err), "~ %s\n%s", fullCmd, out)
	}

	if out := strings.TrimSpace(res.CombinedOut); out != "" {
		l.Printf(out)
	}
	return nil
}

// getEnvVars returns all COCKROACH_* environment variables, in the form
// "key=value".
func getEnvVars() []string {
	var sl []string
	for _, v := range os.Environ() {
		if strings.HasPrefix(v, "COCKROACH_") {
			sl = append(sl, v)
		}
	}
	return sl
}

// SuppressMetamorphicConstantsEnvVar returns the env var to disable metamorphic testing.
// This doesn't actually disable metamorphic constants for a test **unless** it is used
// when starting the cluster. This does however suppress the metamorphic constants being
// logged for every cockroach invocation, which while benign, can be very confusing as the
// constants will be different than what the cockroach cluster is actually using.
//
// TODO(darrylwong): Ideally, the metamorphic constants framework would be smarter and only
// log constants when asked for instead of unconditionally on init(). That way we can remove
// this workaround and just log the constants once when the cluster is started.
func SuppressMetamorphicConstantsEnvVar() string {
	return config.DisableMetamorphicTestingEnvVar
}

// PreStartHook is a hook that is run locally after service registration has completed
// but before any cockroach processes have started in the cluster.
type PreStartHook struct {
	Name    string
	Fn      func(context.Context) error
	Timeout time.Duration
}
