// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package server

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/multitenant/mtinfopb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/clientsecopts"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/server/status"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/storage/fs"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/admission"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/netutil/addr"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// tenantServerCreator is used by the serverController to instantiate
// tenant servers.
type tenantServerCreator interface {
	// newTenantServer instantiates a tenant server.
	//
	// The value provided for index is guaranteed to be different for each
	// simultaneously running server. This can be used to allocate distinct but
	// predictable network listeners.
	//
	// If the specified tenant name is invalid (tenant does not exist or is not
	// active), the returned error will contain the ErrInvalidTenant mark, which
	// can be checked with errors.Is.
	//
	// testArgs is used by tests to tweak the tenant server.
	newTenantServer(ctx context.Context,
		tenantNameContainer *roachpb.TenantNameContainer,
		tenantStopper *stop.Stopper,
		portStartHint int,
		testArgs base.TestSharedProcessTenantArgs,
	) (onDemandServer, error)
}

var _ tenantServerCreator = &topLevelServer{}

// newTenantServer implements the tenantServerCreator interface.
func (s *topLevelServer) newTenantServer(
	ctx context.Context,
	tenantNameContainer *roachpb.TenantNameContainer,
	tenantStopper *stop.Stopper,
	portStartHint int,
	testArgs base.TestSharedProcessTenantArgs,
) (onDemandServer, error) {
	tenantID, tenantReadOnly, err := s.getTenantID(ctx, tenantNameContainer.Get())
	if err != nil {
		return nil, err
	}

	// Use test override for tenant read-only status if provided.
	if testArgs.TenantReadOnly {
		tenantReadOnly = true
	}

	baseCfg, sqlCfg, err := s.makeSharedProcessTenantConfig(ctx, tenantID, tenantNameContainer.Get(), portStartHint,
		tenantStopper, testArgs.Settings, tenantReadOnly)
	if err != nil {
		return nil, err
	}

	// Apply the TestTenantArgs, if any.
	baseCfg.TestingKnobs = testArgs.Knobs

	tenantServer, err := newTenantServerInternal(ctx, baseCfg, sqlCfg, tenantStopper, tenantNameContainer, s.db.AdmissionPacerFactory)
	if err != nil {
		return nil, err
	}

	return &tenantServerWrapper{stopper: tenantStopper, server: tenantServer}, nil
}

type errInvalidTenantMarker struct{}

func (errInvalidTenantMarker) Error() string { return "invalid tenant" }

// ErrInvalidTenant is reported as one of the error marks
// on the error result of newServerFn, i.e. errors.Is(err,
// ErrInvalidTenant) returns true, when the specified tenant
// name does not correspond to a valid tenant: either it does
// not exist, or it is not currently active, or its service mode
// is not shared.
var ErrInvalidTenant error = errInvalidTenantMarker{}

func (s *topLevelServer) getTenantID(
	ctx context.Context, tenantName roachpb.TenantName,
) (roachpb.TenantID, bool, error) {
	var rec *mtinfopb.TenantInfo
	if err := s.sqlServer.internalDB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		var err error
		rec, err = sql.GetTenantRecordByName(ctx, s.cfg.Settings, txn, tenantName)
		return err
	}); err != nil {
		return roachpb.TenantID{}, false, errors.Mark(err, ErrInvalidTenant)
	}

	tenantID, err := roachpb.MakeTenantID(rec.ID)
	if err != nil {
		return roachpb.TenantID{}, false, errors.Mark(
			errors.NewAssertionErrorWithWrappedErrf(err, "stored tenant ID %d does not convert to TenantID", rec.ID),
			ErrInvalidTenant)
	}

	// Check if tenant is read-only (PCR reader tenant).
	readOnlyTenant := rec.ReadFromTenant != nil

	return tenantID, readOnlyTenant, nil
}

// newTenantServerInternal instantiates a server for the given target
// tenant ID.
//
// Note that even if an error is returned, closers may have been
// registered with the stopper, so the caller needs to Stop() it.
func newTenantServerInternal(
	ctx context.Context,
	baseCfg BaseConfig,
	sqlCfg SQLConfig,
	stopper *stop.Stopper,
	tenantNameContainer *roachpb.TenantNameContainer,
	elastic admission.PacerFactory,
) (*SQLServerWrapper, error) {
	ambientCtx := baseCfg.AmbientCtx
	stopper.SetTracer(baseCfg.Tracer)

	// New context, since we're using a separate tracer.
	newCtx := ambientCtx.AnnotateCtx(context.Background())

	// Inform the logs we're starting a new server.
	log.Infof(newCtx, "creating tenant server")

	// Now instantiate the tenant server proper.
	return newSharedProcessTenantServer(newCtx, stopper, baseCfg, sqlCfg, tenantNameContainer, elastic)
}

func (s *topLevelServer) makeSharedProcessTenantConfig(
	ctx context.Context,
	tenantID roachpb.TenantID,
	tenantName roachpb.TenantName,
	portStartHint int,
	stopper *stop.Stopper,
	testSettings *cluster.Settings,
	tenantReadOnly bool,
) (BaseConfig, SQLConfig, error) {
	// Create a configuration for the new tenant.
	parentCfg := s.cfg
	localServerInfo := LocalKVServerInfo{
		InternalServer:                  s.node,
		ServerInterceptors:              s.grpc.serverInterceptorsInfo,
		SameProcessCapabilityAuthorizer: s.rpcContext.TenantRPCAuthorizer,
	}
	st := cluster.MakeClusterSettings()
	if testSettings != nil {
		// If there are testing default overrides in the base config, copy them to the
		// shared process server too.
		st.SV.TestingCopyForVirtualCluster(&testSettings.SV)
	}

	baseCfg, sqlCfg, err := makeSharedProcessTenantServerConfig(ctx, tenantID, tenantName, portStartHint, parentCfg,
		localServerInfo, st, stopper, s.recorder, tenantReadOnly)
	if err != nil {
		return BaseConfig{}, SQLConfig{}, err
	}
	// Inherit the node ID from the server.
	baseCfg.IDContainer.Set(ctx, s.NodeID())
	return baseCfg, sqlCfg, nil
}

func makeSharedProcessTenantServerConfig(
	ctx context.Context,
	tenantID roachpb.TenantID,
	tenantName roachpb.TenantName,
	portStartHint int,
	kvServerCfg Config,
	kvServerInfo LocalKVServerInfo,
	st *cluster.Settings,
	stopper *stop.Stopper,
	nodeMetricsRecorder *status.MetricsRecorder,
	tenantReadOnly bool,
) (baseCfg BaseConfig, sqlCfg SQLConfig, err error) {
	tr := tracing.NewTracerWithOpt(ctx, tracing.WithClusterSettings(&st.SV))

	// Define a tenant store. This will be used to write the
	// listener addresses.
	//
	// First, determine if there's a disk store or whether we will
	// use an in-memory store.
	candidateSpec := kvServerCfg.Stores.Specs[0]
	for _, storeSpec := range kvServerCfg.Stores.Specs {
		if storeSpec.InMemory {
			continue
		}
		candidateSpec = storeSpec
		break
	}
	// Then construct a spec. The logic above either selected an
	// in-memory store (e.g. in tests) or the first on-disk store. In
	// the on-disk case, we reuse the original spec; this propagates
	// all the common store parameters.
	storeSpec := candidateSpec
	if !storeSpec.InMemory {
		storeDir := filepath.Join(storeSpec.Path, "tenant-"+tenantID.String())
		if err := os.MkdirAll(storeDir, 0755); err != nil {
			return BaseConfig{}, SQLConfig{}, err
		}
		stopper.AddCloser(stop.CloserFn(func() {
			if err := os.RemoveAll(storeDir); err != nil {
				log.Warningf(context.Background(), "unable to delete tenant directory: %v", err)
			}
		}))
		storeSpec.Path = storeDir
	}
	baseCfg = MakeBaseConfig(st, tr, storeSpec)

	// Uncontroversial inherited values.
	baseCfg.Config.Insecure = kvServerCfg.Config.Insecure
	baseCfg.Config.User = kvServerCfg.Config.User
	baseCfg.Config.DisableTLSForHTTP = kvServerCfg.Config.DisableTLSForHTTP
	baseCfg.Config.AcceptSQLWithoutTLS = kvServerCfg.Config.AcceptSQLWithoutTLS
	baseCfg.Config.RPCHeartbeatInterval = kvServerCfg.Config.RPCHeartbeatInterval
	baseCfg.Config.RPCHeartbeatTimeout = kvServerCfg.Config.RPCHeartbeatTimeout
	baseCfg.Config.ClockDevicePath = kvServerCfg.Config.ClockDevicePath
	baseCfg.Config.ClusterName = kvServerCfg.Config.ClusterName
	baseCfg.Config.DisableClusterNameVerification = kvServerCfg.Config.DisableClusterNameVerification

	baseCfg.MaxOffset = kvServerCfg.BaseConfig.MaxOffset
	baseCfg.TestingInsecureWebAccess = kvServerCfg.BaseConfig.TestingInsecureWebAccess
	baseCfg.Locality = kvServerCfg.BaseConfig.Locality
	baseCfg.EnableDemoLoginEndpoint = kvServerCfg.BaseConfig.EnableDemoLoginEndpoint
	baseCfg.DefaultZoneConfig = kvServerCfg.BaseConfig.DefaultZoneConfig
	baseCfg.HeapProfileDirName = kvServerCfg.BaseConfig.HeapProfileDirName
	baseCfg.CPUProfileDirName = kvServerCfg.BaseConfig.CPUProfileDirName
	baseCfg.ExecutionTraceDirName = kvServerCfg.BaseConfig.ExecutionTraceDirName
	baseCfg.GoroutineDumpDirName = kvServerCfg.BaseConfig.GoroutineDumpDirName

	// The ListenerFactory allows us to dynamically choose a
	// listen port based on the user's configuration.
	//
	// TODO(knz): use a single network interface for all tenant servers.
	// See: https://github.com/cockroachdb/cockroach/issues/92524
	baseCfg.RPCListenerFactory = ListenerFactoryForConfig(&kvServerCfg.BaseConfig, portStartHint)

	// We reset the port of the KV Addr configuration to 0. If we
	// don't have a customized listener factory, this will force
	// the operating system to choose a port by default.
	baseCfg.Config.Addr, err = resetPort(kvServerCfg.Config.Addr)
	if err != nil {
		return BaseConfig{}, SQLConfig{}, err
	}
	baseCfg.Config.AdvertiseAddr, err = resetPort(kvServerCfg.Config.AdvertiseAddr)
	if err != nil {
		return BaseConfig{}, SQLConfig{}, err
	}

	// The parent server will route HTTP requests to us.
	baseCfg.DisableHTTPListener = true
	// Nevertheless, we like to know our own HTTP address.
	baseCfg.HTTPAddr = kvServerCfg.Config.HTTPAddr
	baseCfg.HTTPAdvertiseAddr = kvServerCfg.Config.HTTPAdvertiseAddr

	// The parent server will route SQL connections to us.
	baseCfg.DisableSQLListener = true
	baseCfg.SplitListenSQL = true
	// Nevertheless, we like to know our own addresses.
	baseCfg.SocketFile = kvServerCfg.Config.SocketFile
	baseCfg.SQLAddr = kvServerCfg.Config.SQLAddr
	baseCfg.SQLAdvertiseAddr = kvServerCfg.Config.SQLAdvertiseAddr

	// Secondary tenant servers need access to the certs
	// directory for two purposes:
	// - to authenticate incoming RPC connections, until
	//   this issue is resolved: https://github.com/cockroachdb/cockroach/issues/92524
	// - to load client certs to present to the remote peer
	//   on outgoing node-node connections.
	//
	// Regarding the second point, we currently still need a client
	// tenant cert to be manually created. Error out if it's not ready.
	// This check can go away when the following issue is resolved:
	// https://github.com/cockroachdb/cockroach/issues/96215
	baseCfg.SSLCertsDir = kvServerCfg.BaseConfig.SSLCertsDir
	baseCfg.SSLCAKey = kvServerCfg.BaseConfig.SSLCAKey

	// Don't let this SQL server take its own background heap/goroutine/CPU profile dumps.
	// The system tenant's SQL server is doing this job.
	baseCfg.DisableRuntimeStatsMonitor = true
	baseCfg.GoroutineDumpDirName = ""
	baseCfg.HeapProfileDirName = ""
	baseCfg.CPUProfileDirName = ""
	baseCfg.ExecutionTraceDirName = ""

	// Expose the process-wide runtime metrics to the tenant's metric
	// collector. Since they are process-wide, all tenants can see them.
	baseCfg.RuntimeStatSampler = kvServerCfg.BaseConfig.RuntimeStatSampler

	// If job trace dumps were enabled for the top-level server, enable
	// them for us too. However, in contrast to temporary files, we
	// don't want them to be deleted when the tenant server shuts down.
	// So we store them into a directory relative to the main trace dump
	// directory.
	if kvServerCfg.BaseConfig.InflightTraceDirName != "" {
		traceDir := filepath.Join(kvServerCfg.BaseConfig.InflightTraceDirName, "tenant-"+tenantID.String())
		if err := os.MkdirAll(traceDir, 0755); err != nil {
			return BaseConfig{}, SQLConfig{}, err
		}
		baseCfg.InflightTraceDirName = traceDir
	}

	tempStorageCfg := base.InheritTempStorageConfig(ctx, st, kvServerCfg.SQLConfig.TempStorageConfig)
	if !tempStorageCfg.InMemory {
		useStore := tempStorageCfg.Spec
		// TODO(knz): Make tempDir configurable.
		tempDir := useStore.Path
		var unlockDirFn func()
		if tempStorageCfg.Path, unlockDirFn, err = fs.CreateTempDir(tempDir, TempDirPrefix); err != nil {
			return BaseConfig{}, SQLConfig{}, errors.Wrap(err, "could not create temporary directory for temp storage")
		}
		stopper.AddCloser(stop.CloserFn(unlockDirFn))
		if useStore.Path != "" {
			recordPath := filepath.Join(useStore.Path, TempDirsRecordFilename)
			if err := fs.RecordTempDir(recordPath, tempStorageCfg.Path); err != nil {
				return BaseConfig{}, SQLConfig{}, errors.Wrap(err, "could not record temp dir")
			}
		}
	}

	sqlCfg = MakeSQLConfig(tenantID, tenantName, tempStorageCfg)
	sqlCfg.TenantReadOnly = tenantReadOnly
	baseCfg.ExternalIODirConfig = kvServerCfg.BaseConfig.ExternalIODirConfig

	baseCfg.ExternalIODir = kvServerCfg.BaseConfig.ExternalIODir

	// Use the internal connector instead of the network.
	// See: https://github.com/cockroachdb/cockroach/issues/84591
	sqlCfg.TenantLoopbackAddr = kvServerCfg.BaseConfig.Config.AdvertiseAddr

	// Use the same memory budget for each secondary tenant. The assumption
	// here is that we use max 2 tenants, and that under common loads one
	// of them will be mostly idle.
	// We might want to reconsider this if we use more than 1 in-memory tenant at a time.
	sqlCfg.MemoryPoolSize = kvServerCfg.SQLConfig.MemoryPoolSize
	sqlCfg.TableStatCacheSize = kvServerCfg.SQLConfig.TableStatCacheSize
	sqlCfg.QueryCacheSize = kvServerCfg.SQLConfig.QueryCacheSize

	// LocalKVServerInfo tells the rpc.Context of the tenant's server
	// that it is inside the same process as the KV layer and how to
	// reach this KV layer without going through the network.
	sqlCfg.LocalKVServerInfo = &kvServerInfo

	sqlCfg.NodeMetricsRecorder = nodeMetricsRecorder
	sqlCfg.LicenseEnforcer = kvServerCfg.SQLConfig.LicenseEnforcer

	return baseCfg, sqlCfg, nil
}

func resetPort(addrToChange string) (string, error) {
	h, _, err := addr.SplitHostPort(addrToChange, "0")
	if err != nil {
		return "", err
	}

	return net.JoinHostPort(h, "0"), nil
}

func (s *SQLServerWrapper) reportTenantInfo(ctx context.Context) error {
	var buf redact.StringBuilder
	buf.Printf("started tenant SQL server at %s\n", timeutil.Now())
	buf.Printf("webui:\t%s\n", s.cfg.AdminURL())
	clientConnOptions, serverParams := MakeServerOptionsForURL(s.cfg.Config)
	pgURL, err := clientsecopts.MakeURLForServer(clientConnOptions, serverParams, url.User(username.RootUser))
	if err != nil {
		log.Errorf(ctx, "failed computing the URL: %v", err)
	} else {
		buf.Printf("sql:\t%s\n", pgURL.ToPQ())
		buf.Printf("sql (JDBC):\t%s\n", pgURL.ToJDBC())
	}
	if s.cfg.SocketFile != "" {
		buf.Printf("socket:\t%s\n", s.cfg.SocketFile)
	}
	if tmpDir := s.sqlCfg.TempStorageConfig.Path; tmpDir != "" {
		buf.Printf("temp dir:\t%s\n", tmpDir)
	}
	buf.Printf("clusterID:\t%s\n", s.cfg.ClusterIDContainer.Get())
	buf.Printf("tenantID:\t%s\n", s.sqlCfg.TenantID)
	buf.Printf("instanceID:\t%d\n", s.cfg.IDContainer.Get())
	// Collect the formatted string and show it to the user.
	msg, err := util.ExpandTabsInRedactableBytes(buf.RedactableBytes())
	if err != nil {
		return err
	}
	msgS := msg.ToString()
	log.Ops.Infof(ctx, "tenant startup completed:\n%s", msgS)
	return nil
}
