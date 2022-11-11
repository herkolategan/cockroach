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
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachprod-bench/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachprod-bench/google"
	"github.com/cockroachdb/cockroach/pkg/roachprod"
	"github.com/cockroachdb/cockroach/pkg/roachprod/logger"
	"github.com/cockroachdb/cockroach/pkg/roachprod/ssh"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/oserror"
	"golang.org/x/perf/benchstat"
)

const timeFormat = "2006-01-02T15_04_05"

// TODO: Setup Google Cloud Storage
// TODO: Setup temporary folder for copy operations (google golang howto temp)
// Do compare in temp folder.
// publish can publish to gs:// or local file system

var (
	l                *logger.Logger
	flags            = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	flagCopy         = flags.Bool("copy", true, "copy and extract roachbench artifacts and libraries to the target cluster")
	flagLibDir       = flags.String("libdir", "lib.docker_amd64", "location of libraries required by test binaries")
	flagRemoteDir    = flags.String("remotedir", "/mnt/data1", "roachbench working directory on the target cluster")
	flagFailFast     = flags.Bool("failfast", false, "fail fast if any of the roachprod commands fail")
	flagCompareDir   = flags.String("comparedir", "", "directory with reports to compare the results of the benchmarks against")
	flagPublishDir   = flags.String("publishdir", "", "directory to publish the reports of the benchmarks to")
	flagPreviousTime = flags.String("previoustime", "", "timestamp of the previous run to compare against")
	flagCluster      = flags.String("cluster", "", "cluster to run the benchmarks on")
	logOutputDir     string
	benchDir         string
	timestamp        time.Time
	testArgs         []string
)

type benchmark struct {
	pkg  string
	name string
}

func verifyArtifactsExist(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return errors.Wrapf(err, "the benchdir flag %q is not a directory relative to the current working directory", dir)
	}
	if !fi.Mode().IsDir() {
		return fmt.Errorf("the benchdir flag %q is not a directory relative to the current working directory", dir)
	}
	return nil
}

func initRoachprod() {
	_ = roachprod.InitProviders()
	if _, err := roachprod.Sync(l); err != nil {
		l.Printf("Failed to sync roachprod data - %v", err)
		os.Exit(1)
	}
}

func logResponseErrors(response cluster.RemoteResponse) {
	if response.Err != nil {
		l.Errorf("Remote command = {%s}, error = {%v}, stderr output = {%s}",
			strings.Join(response.Args, " "), response.Err, response.Stderr)
	}
}

func setupVars() error {
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "usage: %s <benchdir> [<flags>] -- [<args>]\n", flags.Name())
		flags.PrintDefaults()
	}

	if len(os.Args) < 2 {
		var b bytes.Buffer
		flags.SetOutput(&b)
		flags.Usage()
		return errors.Newf("%s", b.String())
	}

	testArgs = getTestArgs()
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}

	if *flagPreviousTime == "" {
		timestamp = timeutil.Now()
		if *flagCluster == "" {
			return fmt.Errorf("the cluster flag is required if not comparing against a previous run")
		}
	} else {
		var err error
		timestamp, err = time.Parse(timeFormat, *flagPreviousTime)
		if err != nil {
			return err
		}
	}

	benchDir = strings.TrimRight(os.Args[1], "/")
	logOutputDir = filepath.Join(benchDir, fmt.Sprintf("logs-%s", timestamp.Format(timeFormat)))

	err := verifyArtifactsExist(benchDir)
	if err != nil {
		return err
	}

	return nil
}

func roachprodRun(clusterName string, cmdArray []string) error {
	return roachprod.Run(context.Background(), l, clusterName, "", "", false, os.Stdout, os.Stderr, cmdArray)
}

// TODO: add iterations
func executeBenchmarks(packages []string) error {
	initLogger()
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
			[]string{"sh", "-c",
				fmt.Sprintf("\"cd %s/%s/bin && ./run.sh -test.list=Benchmark*\"",
					remoteDir, pkg)},
			pkg,
		}
		commands = append(commands, command)
	}

	l.Printf("Distributing and running benchmark listings across cluster %s\n", *flagCluster)
	isValidBenchmarkName := regexp.MustCompile(`^Benchmark[a-zA-Z0-9_]+$`).MatchString
	errorCount := 0
	benchmarks := make([]benchmark, 0)
	counts := make(map[string]int)
	cluster.ExecuteRemoteCommands(l, *flagCluster, commands, numNodes, *flagFailFast, func(response cluster.RemoteResponse) {
		fmt.Print(".")
		logResponseErrors(response)
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
		}
		if response.Err != nil {
			errorCount++
		}
	})

	if errorCount > 0 && *flagFailFast {
		return errors.Newf("Failed to list benchmarks")
	}

	fmt.Println()
	for pkg, count := range counts {
		l.Printf("Found %d benchmarks in %s\n", count, pkg)
	}

	// Generate commands for running benchmarks.
	commands = make([]cluster.RemoteCommand, 0)
	for _, bench := range benchmarks {
		shellCommand := fmt.Sprintf("\"(rm -rf /tmp/* || true) && cd %s/%s/bin && "+
			"./run.sh %s -test.benchmem -test.bench=^%s$ -test.run=^$\"",
			remoteDir, bench.pkg, strings.Join(testArgs, " "), bench.name)
		command := cluster.RemoteCommand{
			[]string{"sh", "-c", shellCommand},
			bench,
		}
		commands = append(commands, command)
	}

	logIndex := 0
	missingBenchmarks := make([]benchmark, 0)
	l.Printf("Found %d benchmarks, distributing and running benchmarks across cluster %s\n", len(benchmarks), *flagCluster)
	cluster.ExecuteRemoteCommands(l, *flagCluster, commands, numNodes, *flagFailFast, func(response cluster.RemoteResponse) {
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

func run() error {
	ctx := context.Background()
	if err := setupVars(); err != nil {
		return err
	}
	packages, err := readManifest(benchDir)
	if err != nil {
		return err
	}

	if *flagPreviousTime == "" {
		err = executeBenchmarks(packages)
		if err != nil {
			return err
		}
	}

	if *flagCompareDir != "" {
		service, err := google.New(ctx)
		if err != nil {
			return err
		}
		processResult := func(pkgGroup string, tables []*benchstat.Table) error {
			return publishToGoogleSheets(ctx, service, pkgGroup+"/...", tables)
		}
		err = compareBenchmarks(packages, *flagCompareDir, logOutputDir, processResult)
		if err != nil {
			return err
		}
	} else {
		fmt.Println("No comparison directory specified, skipping comparison")
	}

	return nil
}

func main() {
	ssh.InsecureIgnoreHostKey = true
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		fmt.Println("FAIL")
		os.Exit(1)
	} else {
		fmt.Println("SUCCESS")
	}
}

func getTestArgs() (ret []string) {
	if len(os.Args) > 2 {
		flagsAndArgs := os.Args[2:]
		for i, arg := range flagsAndArgs {
			if arg == "--" {
				ret = flagsAndArgs[i+1:]
				break
			}
		}
	}
	return ret
}
