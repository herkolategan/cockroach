// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package main

import (
	"log"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachprod-microbench/parser"
	"github.com/cockroachdb/errors"
)

func (b *Benchmark) command(revision Revision, iterType BenchmarkIterationType) *exec.Cmd {
	cmd := exec.Command(
		path.Join(suite.binDir(revision), b.binaryName()),
		b.args(suite.artifactsDir(revision), iterType)...,
	)
	cmd.Env = append(os.Environ(), "COCKROACH_RANDOM_SEED=1")
	return cmd
}

func (b *Benchmark) runIteration(revision Revision) error {
	cmd := b.command(revision, Measure)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "benchmark %q, command %q failed to run:\n%s",
			b.DisplayName, cmd.String(), string(output))
	}

	results := parser.ExtractBenchmarkResults(string(output))
	if results.Errors {
		return errors.Newf("benchmark results contained errors:\n%s", string(output))
	}
	if results.Skipped {
		return errors.Newf("benchmark invalid due to being skipped:\n%s", string(output))
	}

	var cleanedOutput strings.Builder
	for _, result := range results.Results {
		cleanedOutput.WriteString(strings.Join(result, " "))
		cleanedOutput.WriteString("\n")
	}
	err = appendToFile(path.Join(suite.artifactsDir(revision), b.cleanLog()), cleanedOutput.String())
	if err != nil {
		return err
	}
	err = appendToFile(path.Join(suite.artifactsDir(revision), b.rawLog()), string(output))
	if err != nil {
		return err
	}
	return nil
}

func (b *Benchmark) runProfiles(revision Revision) error {
	for _, profile := range []BenchmarkIterationType{ProfileCPU, ProfileMemory, ProfileMutex} {
		cmd := b.command(revision, profile)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return errors.Wrapf(err, "profile benchmark %q, command %q failed to run:\n%s",
				b.DisplayName, cmd.String(), string(output))
		}
	}
	return nil
}

func (b *Benchmark) run() error {
	for _, revision := range []Revision{New, Old} {
		err := os.MkdirAll(suite.artifactsDir(revision), 0755)
		if err != nil {
			return err
		}
	}

	log.Printf("Running benchmark %q for %d iterations", b.Name, b.MeasureCount)
	for i := 0; i < b.MeasureCount; i++ {
		for _, revision := range []Revision{New, Old} {
			log.Printf("%s binary iteration (%d out of %d)",
				revision, i+1, b.MeasureCount,
			)
			err := b.runIteration(revision)
			if err != nil {
				return err
			}
		}
	}

	// Only collect profiles if there was a regression.
	compareResult, err := b.compare()
	if err != nil {
		return err
	}
	if compareResult.regressed() {
		for _, revision := range []Revision{New, Old} {
			log.Printf("collecting profiles for revision %s", revision)
			err := b.runProfiles(revision)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func appendToFile(filename, data string) error {
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.WriteString(data); err != nil {
		return err
	}
	return nil
}

func (b Benchmarks) run() error {
	for _, benchmark := range b {
		if config.Group != 0 && benchmark.RunnerGroup != config.Group {
			continue
		}
		if err := benchmark.run(); err != nil {
			return err
		}
	}
	return nil
}
