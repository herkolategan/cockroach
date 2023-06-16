package tests

import (
	"context"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/option"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/stretchr/testify/require"
	"time"
)

func registerMultiTenantWorking(r registry.Registry) {
	r.Add(registry.TestSpec{
		//Skip:    "the test is skipped until #100260 is resolved",
		Name:    "multitenant/working",
		Owner:   registry.OwnerSQLQueries,
		Cluster: r.MakeClusterSpec(3),
		Leases:  registry.MetamorphicLeases,
		Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
			runMultiTenantWorking(ctx, t, c)
		},
	})
}

func runMultiTenantWorking(
	ctx context.Context,
	t test.Test,
	c cluster.Cluster,
) {
	c.Put(ctx, t.Cockroach(), "./cockroach")

	settings := install.MakeClusterSettings(install.SecureOption(true))
	c.Start(ctx, t.L(), option.DefaultStartOpts(), settings, c.Node(1))
	c.Start(ctx, t.L(), option.DefaultStartOpts(), settings, c.Node(2))
	c.Start(ctx, t.L(), option.DefaultStartOpts(), settings, c.Node(3))

	const (
		tenantID           = 11
		tenantBaseHTTPPort = 8081
		tenantBaseSQLPort  = 26259 + 1000
	)

	numInstances := 3
	tenantHTTPPort := func(offset int) int {
		if c.IsLocal() || numInstances > c.Spec().NodeCount {
			return tenantBaseHTTPPort + offset
		}
		return tenantBaseHTTPPort
	}
	tenantSQLPort := func(offset int) int {
		if c.IsLocal() || numInstances > c.Spec().NodeCount {
			return tenantBaseSQLPort + offset
		}
		return tenantBaseSQLPort
	}

	storConn := c.Conn(ctx, t.L(), 1)
	_, err := storConn.Exec(`SELECT crdb_internal.create_tenant($1::INT)`, tenantID)
	require.NoError(t, err)

	t.L().Printf("creating tenant node on sql port %d\n", tenantSQLPort(0))
	tenant := createTenantNode(ctx, t, c, c.All(), tenantID, 2 /* node */, tenantHTTPPort(0), tenantSQLPort(0),
		createTenantCertNodes(c.All()))
	defer tenant.stop(ctx, t, c)
	tenant.start(ctx, t, c, "./cockroach")

	time.Sleep(5000 * time.Second)
}
