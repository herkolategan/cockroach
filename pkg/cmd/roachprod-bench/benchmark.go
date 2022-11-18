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
	"github.com/cockroachdb/cockroach/pkg/cmd/roachprod-bench/cluster"
	"github.com/cockroachdb/cockroach/pkg/roachprod"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/oserror"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// TODO: Split Parts List & Exec
// TODO: Regex exclude list
// TODO: Fail Fast Fix, (set iterations on tests low to test this), Log Issue
// TODO: Prevent runs on packages with zero and fail if no packages are found / benchmarks are found
// Add script templates?

// TODO: add iterations
// executeBenchmarks executes the microbenchmarks on the target cluster. Output is written to the log output directory.
// A logger is also initialised log to output to the log output directory.
func executeBenchmarks(packages []string) error {
	initRoachprod()
	buildHash := filepath.Base(benchDir)

	err := createReports(packages)
	if err != nil {
		return err
	}
	defer closeReports()

	// Locate binaries tarball.
	ext := "tar"
	if _, osErr := os.Stat(filepath.Join(benchDir, "bin.tar.gz")); osErr == nil {
		ext = "tar.gz"
	}
	if _, osErr := os.Stat(filepath.Join(benchDir, fmt.Sprintf("bin.%s", ext))); oserror.IsNotExist(osErr) {
		l.Errorf("bin.%s does not exist in %s", ext, benchDir)
		return osErr
	}
	binPath := fmt.Sprintf("%s/bin.%s", benchDir, ext)
	remoteBinName := fmt.Sprintf("roachbench-%s.%s", buildHash, ext)
	remoteDir := fmt.Sprintf("%s/roachbench-%s", *flagRemoteDir, buildHash)

	if *flagCopy {
		if fi, cmdErr := os.Stat(*flagLibDir); cmdErr == nil && fi.IsDir() {
			if putErr := roachprod.Put(context.Background(), l, *flagCluster, *flagLibDir, "lib", true); putErr != nil {
				return putErr
			}
		}
		if err = roachprod.Put(context.Background(), l, *flagCluster, binPath, remoteBinName, true); err != nil {
			return err
		}

		// Clear old build artifacts.
		if err = roachprodRun(*flagCluster, []string{"rm", "-rf", remoteDir}); err != nil {
			return errors.Wrap(err, "failed to remove old build artifacts")
		}

		// Copy and extract new build artifacts.
		if err = roachprodRun(*flagCluster, []string{"mkdir", "-p", remoteDir}); err != nil {
			return err
		}
		extractFlags := "-xf"
		if ext == "tar.gz" {
			extractFlags = "-xzf"
		}
		if err = roachprodRun(*flagCluster, []string{"tar", extractFlags, remoteBinName, "-C", remoteDir}); err != nil {
			return err
		}
	}

	// Generate commands for listing benchmarks.
	statuses, err := roachprod.Status(context.Background(), l, *flagCluster, "")
	if err != nil {
		return err
	}
	numNodes := len(statuses)
	commands := make([]cluster.RemoteCommand, 0)
	for _, pkg := range packages {
		command := cluster.RemoteCommand{
			Args: []string{"sh", "-c",
				fmt.Sprintf("\"cd %s/%s/bin && ./run.sh -test.list=Benchmark*\"",
					remoteDir, pkg)},
			Metadata: pkg,
		}
		commands = append(commands, command)
	}

	l.Printf("Distributing and running benchmark listings across cluster %s\n", *flagCluster)
	isValidBenchmarkName := regexp.MustCompile(`^Benchmark[a-zA-Z0-9_]+$`).MatchString
	errorCount := 0
	benchmarks := make([]benchmark, 0)
	counts := make(map[string]int)
	cluster.ExecuteRemoteCommands(l, *flagCluster, commands, numNodes, true, func(response cluster.RemoteResponse) {
		fmt.Print(".")
		if response.Err == nil {
			pkg := response.Metadata.(string)
			for _, benchmarkName := range strings.Split(response.Stdout, "\n") {
				benchmarkName = strings.TrimSpace(benchmarkName)
				if isValidBenchmarkName(benchmarkName) {
					benchmarks = append(benchmarks, benchmark{pkg, benchmarkName})
					counts[pkg]++
				} else if benchmarkName != "" {
					l.Printf("Ignoring invalid benchmark name: %s", benchmarkName)
				}
			}
		} else {
			fmt.Println()
			l.Errorf("Remote command = {%s}, error = {%v}, stderr output = {%s}",
				strings.Join(response.Args, " "), response.Err, response.Stderr)
			errorCount++
		}
	})

	if errorCount > 0 {
		return errors.Newf("Failed to list benchmarks")
	}

	fmt.Println()
	for pkg, count := range counts {
		l.Printf("Found %d benchmarks in %s\n", count, pkg)
	}

	// Generate commands for running benchmarks.
	commands = make([]cluster.RemoteCommand, 0)
	for _, bench := range benchmarks {
		shellCommand := fmt.Sprintf("\"cd %s/%s/bin && ./run.sh %s -test.benchmem -test.bench=^%s$ -test.run=^$\"",
			remoteDir, bench.pkg, strings.Join(testArgs, " "), bench.name)
		command := cluster.RemoteCommand{
			Args:     []string{"sh", "-c", shellCommand},
			Metadata: bench,
		}
		commands = append(commands, command)
	}

	logIndex := 0
	missingBenchmarks := make([]benchmark, 0)
	l.Printf("Found %d benchmarks, distributing and running benchmarks across cluster %s\n", len(benchmarks), *flagCluster)
	cluster.ExecuteRemoteCommands(l, *flagCluster, commands, numNodes, !*flagLenient, func(response cluster.RemoteResponse) {
		fmt.Print(".")
		benchmarkResults, containsErrors := extractBenchmarkResults(response.Stdout)
		benchmarkResponse := response.Metadata.(benchmark)
		for _, benchmarkResult := range benchmarkResults {
			if _, writeErr := benchmarkOutput[benchmarkResponse.pkg].WriteString(
				fmt.Sprintf("%s\n", strings.Join(benchmarkResult, " "))); writeErr != nil {
				l.Errorf("Failed to write benchmark result to file - %v", writeErr)
			}
		}
		if containsErrors || response.Err != nil {
			fmt.Println()
			err = writeBenchmarkErrorLogs(response, logIndex)
			if err != nil {
				l.Errorf("Failed to write error logs - %v", err)
			}
			errorCount++
			logIndex++
		}
		if _, writeErr := analyticsOutput[benchmarkResponse.pkg].WriteString(
			fmt.Sprintf("%s %d ms\n", benchmarkResponse.name,
				response.Duration.Milliseconds())); writeErr != nil {
			l.Errorf("Failed to write analytics to file - %v", writeErr)
		}
		if len(benchmarkResults) == 0 {
			missingBenchmarks = append(missingBenchmarks, benchmarkResponse)
		}
	})

	fmt.Println()
	l.Printf("Completed benchmarks, results located at %s for time %s\n", logOutputDir, timestamp.Format(timeFormat))
	if len(missingBenchmarks) > 0 {
		l.Errorf("Failed to find results for %d benchmarks", len(missingBenchmarks))
		l.Errorf("Missing benchmarks %v", missingBenchmarks)
	}
	if errorCount != 0 {
		return errors.Newf("Found %d errors during remote execution", errorCount)
	}
	return nil
}
