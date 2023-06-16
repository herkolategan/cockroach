// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package roachprod

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/cockroach/pkg/roachprod/logger"
	"github.com/cockroachdb/errors"
)

// StartTenant starts nodes on a cluster in "tenant" mode (each node is a SQL
// instance). A tenant cluster needs an existing, running host cluster. The
// tenant metadata is created on the host cluster if it doesn't exist already.
//
// The host and tenant can use the same underlying cluster, as long as different
// subsets of nodes are selected (e.g. "local:1,2" and "local:3,4").
func StartTenant(
	ctx context.Context,
	l *logger.Logger,
	tenantCluster string,
	hostCluster string,
	startOpts install.StartOpts,
	clusterSettingsOpts ...install.ClusterSettingOption,
) error {
	tc, err := newCluster(l, tenantCluster, clusterSettingsOpts...)
	if err != nil {
		return err
	}

	// TODO(radu): do we need separate clusterSettingsOpts for the host cluster?
	hc, err := newCluster(l, hostCluster, clusterSettingsOpts...)
	if err != nil {
		return err
	}

	if tc.Name == hc.Name {
		// We allow using the same cluster, but the node sets must be disjoint.
		for _, n1 := range tc.Nodes {
			for _, n2 := range hc.Nodes {
				if n1 == n2 {
					return errors.Errorf("host and tenant nodes must be disjoint")
				}
			}
		}
	}

	startOpts.Target = install.StartTenantSQL
	if startOpts.TenantID < 2 {
		return errors.Errorf("invalid tenant ID %d (must be 2 or higher)", startOpts.TenantID)
	}

	// Create tenant, if necessary. We need to run this SQL against a single host,
	// so temporarily restrict the target nodes to 1.
	saveNodes := hc.Nodes
	hc.Nodes = hc.Nodes[:1]
	l.Printf("Creating tenant metadata")
	if err := hc.ExecSQL(ctx, l, "", 0, []string{
		`-e`,
		fmt.Sprintf(createTenantIfNotExistsQuery, startOpts.TenantID),
	}); err != nil {
		return err
	}
	hc.Nodes = saveNodes

	var kvAddrs []string
	for _, node := range hc.Nodes {
		kvAddrs = append(kvAddrs, fmt.Sprintf("%s:%d", hc.Host(node), hc.NodePort(node)))
	}
	startOpts.KVAddrs = strings.Join(kvAddrs, ",")
	startOpts.KVCluster = hc
	return tc.Start(ctx, l, startOpts)
}

// createTenantIfNotExistsQuery is used to initialize the tenant metadata, if
// it's not initialized already. We set up the tenant with a lot of initial RUs
// so that we don't encounter throttling by default.
const createTenantIfNotExistsQuery = `
SELECT
  CASE (SELECT 1 FROM system.tenants WHERE id = %[1]d) IS NULL
  WHEN true
  THEN (
    crdb_internal.create_tenant(%[1]d),
    crdb_internal.update_tenant_resource_limits(%[1]d, 1000000000, 10000, 0, now(), 0)
  )::STRING
  ELSE 'already exists'
  END;`

// EXPERIMENTAL
///////////////

func StartExternal(
	ctx context.Context,
	l *logger.Logger,
	clusterName string,
	startOpts install.StartOpts,
	clusterSettingsOpts ...install.ClusterSettingOption,
) error {
	c, err := newCluster(l, clusterName, clusterSettingsOpts...)
	if err != nil {
		return err
	}
	startOpts.Target = install.StartTenantSQL
	if startOpts.TenantID < 2 {
		return errors.Errorf("invalid tenant ID %d (must be 2 or higher)", startOpts.TenantID)
	}

	// Create tenant, if necessary. We need to run this SQL against a single host,
	// so temporarily restrict the target nodes to 1.
	saveNodes := c.Nodes
	c.Nodes = c.Nodes[:1]
	l.Printf("Creating tenant metadata")
	if err := c.ExecSQL(ctx, l, "", 0, []string{
		`-e`,
		fmt.Sprintf(createTenantIfNotExistsQuery, startOpts.TenantID),
	}); err != nil {
		return err
	}
	c.Nodes = saveNodes

	var kvAddrs []string
	for _, node := range c.Nodes {
		kvAddrs = append(kvAddrs, fmt.Sprintf("%s:%d", c.Host(node), c.NodePort(node)))
	}
	startOpts.KVAddrs = strings.Join(kvAddrs, ",")
	startOpts.KVCluster = c
	return c.Start(ctx, l, startOpts)
}

func StartShared(ctx context.Context,
	l *logger.Logger,
	clusterName, tenantName string,
	clusterSettingsOpts ...install.ClusterSettingOption,
) error {
	hc, err := newCluster(l, clusterName, clusterSettingsOpts...)
	if err != nil {
		return err
	}
	// Create tenant, if necessary. We need to run this SQL against a single host,
	// so temporarily restrict the target nodes to 1.
	saveNodes := hc.Nodes
	hc.Nodes = hc.Nodes[:1]
	l.Printf("Creating tenant metadata")
	if err := hc.ExecSQL(ctx, l, "", 0, []string{
		`-e`,
		fmt.Sprintf(createSharedTenantQuery, tenantName),
	}); err != nil {
		return err
	}
	hc.Nodes = saveNodes

	return nil
}

const createSharedTenantQuery = `
	CREATE TENANT '%[1]s';
	ALTER TENANT '%[1]s' START SERVICE SHARED;
	ALTER TENANT '%[1]s' GRANT CAPABILITY can_view_node_info=true, can_admin_split=true,can_view_tsdb_metrics=true;
	ALTER TENANT '%[1]s' SET CLUSTER SETTING sql.split_at.allow_for_secondary_tenant.enabled=true;
	ALTER TENANT '%[1]s' SET CLUSTER SETTING sql.scatter.allow_for_secondary_tenant.enabled=true;
`
