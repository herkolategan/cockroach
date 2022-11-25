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

// TODO: Regex exclude list
// TODO: add iterations

// TODO: Fail Fast Fix, (set iterations on tests low to test this), Log Issue
// TODO: Prevent runs on packages with zero and fail if no packages are found / benchmarks are found
// Add script templates?
// TODO: only open reports for packages that have benchmarks

// execute benchmarks return valid packages (containing more than zero), benchmarks list
// google how to document a function

// executeBenchmarks executes the microbenchmarks on the target cluster. Output is written to the log output directory.
// A logger is also initialised log to output to the log output directory.

func prepareCluster(buildHash, remoteDir string) (int, error) {
	initRoachprod()

	statuses, err := roachprod.Status(context.Background(), l, *flagCluster, "")
	if err != nil {
		return -1, err
	}
	numNodes := len(statuses)
	l.Printf("Number of nodes %d", numNodes)

	// Locate binaries tarball.
	ext := "tar"
	if _, osErr := os.Stat(filepath.Join(benchDir, "bin.tar.gz")); osErr == nil {
		ext = "tar.gz"
	}
	if _, osErr := os.Stat(filepath.Join(benchDir, fmt.Sprintf("bin.%s", ext))); oserror.IsNotExist(osErr) {
		l.Errorf("bin.%s does not exist in %s", ext, benchDir)
		return -1, osErr
	}
	binPath := fmt.Sprintf("%s/bin.%s", benchDir, ext)
	remoteBinName := fmt.Sprintf("roachbench-%s.%s", buildHash, ext)

	// Clear old artifacts, copy and extract new artifacts on to the cluster.
	if *flagCopy {
		if fi, cmdErr := os.Stat(*flagLibDir); cmdErr == nil && fi.IsDir() {
			if putErr := roachprod.Put(context.Background(), l, *flagCluster, *flagLibDir, "lib", true); putErr != nil {
				return numNodes, putErr
			}
		}
		if rpErr := roachprod.Put(context.Background(), l, *flagCluster, binPath, remoteBinName, true); rpErr != nil {
			return numNodes, rpErr
		}
		if rpErr := roachprodRun(*flagCluster, []string{"rm", "-rf", remoteDir}); rpErr != nil {
			return numNodes, errors.Wrap(rpErr, "failed to remove old build artifacts")
		}
		if rpErr := roachprodRun(*flagCluster, []string{"mkdir", "-p", remoteDir}); rpErr != nil {
			return numNodes, rpErr
		}
		extractFlags := "-xf"
		if ext == "tar.gz" {
			extractFlags = "-xzf"
		}
		if rpErr := roachprodRun(*flagCluster, []string{"tar", extractFlags, remoteBinName, "-C", remoteDir}); rpErr != nil {
			return numNodes, rpErr
		}
	}
	return numNodes, nil
}

func listBenchmarks(remoteDir string, packages []string, numNodes int) ([]string, []benchmark, error) {
	exclusionPairs := getRegexExclusionPairs()

	// Generate commands for listing benchmarks.
	commands := make([]cluster.RemoteCommand, 0)
	for _, pkg := range packages {
		command := cluster.RemoteCommand{
			Args: []string{"sh", "-c",
				fmt.Sprintf("\"cd %s/%s/bin && ./run.sh -test.list=^Benchmark*\"",
					remoteDir, pkg)},
			Metadata: pkg,
		}
		commands = append(commands, command)
	}

	// Execute commands for listing benchmarks.
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
					exclude := false
					for _, exclusionPair := range exclusionPairs {
						if exclusionPair[0].MatchString(pkg) && exclusionPair[1].MatchString(benchmarkName) {
							exclude = true
						}
					}
					if !exclude {
						benchmarks = append(benchmarks, benchmark{pkg, benchmarkName})
						counts[pkg]++
					}
				} else if benchmarkName != "" {
					fmt.Println()
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
		return nil, nil, errors.Newf("Failed to list benchmarks")
	}

	fmt.Println()
	validPackages := make([]string, 0, len(counts))
	for pkg, count := range counts {
		l.Printf("Found %d benchmarks in %s\n", count, pkg)
		validPackages = append(validPackages, pkg)
	}
	return validPackages, benchmarks, nil
}

func executeBenchmarks(packages []string) error {
	buildHash := filepath.Base(benchDir)
	remoteDir := fmt.Sprintf("%s/roachbench-%s", *flagRemoteDir, buildHash)

	numNodes, err := prepareCluster(buildHash, remoteDir)
	if err != nil {
		return err
	}

	validPackages, benchmarks, err := listBenchmarks(remoteDir, packages, numNodes)
	if err != nil {
		return err
	}
	if len(validPackages) == 0 {
		return errors.New("no packages containing benchmarks found")
	}

	err = createReports(validPackages)
	if err != nil {
		return err
	}
	defer closeReports()

	// Generate commands for running benchmarks.
	commands := make([]cluster.RemoteCommand, 0)
	for _, bench := range benchmarks {
		shellCommand := fmt.Sprintf("\"cd %s/%s/bin && ./run.sh %s -test.benchmem -test.bench=^%s$ -test.run=^$\"",
			remoteDir, bench.pkg, strings.Join(testArgs, " "), bench.name)
		command := cluster.RemoteCommand{
			Args:     []string{"sh", "-c", shellCommand},
			Metadata: bench,
		}
		for i := 0; i < *flagIterations; i++ {
			commands = append(commands, command)
		}
	}

	errorCount := 0
	logIndex := 0
	missingBenchmarks := make([]benchmark, 0)
	l.Printf("Found %d benchmarks, distributing and running benchmarks for %d iteration(s) across cluster %s\n",
		len(benchmarks), *flagIterations, *flagCluster)
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
		if *flagLenient {
			l.Printf("Ignoring errors in benchmark results (lenient flag was set)")
		} else {
			return errors.Newf("Found %d errors during remote execution", errorCount)
		}
	}
	return nil
}

func getRegexExclusionPairs() [][]*regexp.Regexp {
	if *flagExclude == "" {
		return nil
	}
	excludeRegexes := make([][]*regexp.Regexp, 0)
	excludeList := strings.Split(*flagExclude, ",")
	for _, pair := range excludeList {
		pairSplit := strings.Split(pair, ":")
		if len(pairSplit) != 2 {
			pkgRegex := regexp.MustCompile(".*")
			benchRegex := regexp.MustCompile(pairSplit[0])
			excludeRegexes = append(excludeRegexes, []*regexp.Regexp{pkgRegex, benchRegex})
		} else {
			pkgRegex := regexp.MustCompile(pairSplit[0])
			benchRegex := regexp.MustCompile(pairSplit[1])
			excludeRegexes = append(excludeRegexes, []*regexp.Regexp{pkgRegex, benchRegex})
		}
	}
	return excludeRegexes
}
