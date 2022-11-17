// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachprod-bench/google"
	"golang.org/x/perf/benchstat"
	"golang.org/x/perf/storage/benchfmt"
)

// TODO: compare dir can be a gs url ... need to be able to handle...

func compareBenchmarks(
	packages []string,
	currentDir, previousDir string,
	outputResult func(pkgGroup string, tables []*benchstat.Table) error, // TODO: just return the results... no need for function anymore
) error {
	packageResults := make(map[string][][]*benchfmt.Result)
	for _, pkg := range packages {
		basePackage := pkg[:strings.Index(pkg[4:]+"/", "/")+4]
		results, ok := packageResults[basePackage]
		if !ok {
			results = [][]*benchfmt.Result{make([]*benchfmt.Result, 0), make([]*benchfmt.Result, 0)}
			packageResults[basePackage] = results
		}
		if err := addReportFile(&results[0], filepath.Join(previousDir, getReportLogName(reportLogName, pkg))); err != nil {
			return err
		}
		if err := addReportFile(&results[1], filepath.Join(currentDir, getReportLogName(reportLogName, pkg))); err != nil {
			return err
		}
	}

	for pkgGroup, results := range packageResults {
		var c benchstat.Collection
		c.Alpha = 0.05
		c.Order = benchstat.Reverse(benchstat.ByDelta)
		c.AddResults("new", results[0])
		c.AddResults("old", results[1])
		tables := c.Tables()
		if err := outputResult(pkgGroup, tables); err != nil {
			return err
		}
	}

	return nil
}

func publishToGoogleSheets(
	ctx context.Context, srv *google.Service, sheetName string, tables []*benchstat.Table,
) error {
	url, err := srv.CreateSheet(ctx, sheetName, tables)
	if err != nil {
		return err
	}
	fmt.Printf("\ngenerated sheet for %s: %s\n", sheetName, url)
	return nil
}

func addReportFile(results *[]*benchfmt.Result, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "error closing file: %s\n", closeErr)
		}
	}()

	br := benchfmt.NewReader(io.Reader(f))
	for br.Next() {
		*results = append(*results, br.Result())
	}
	return br.Err()
}
