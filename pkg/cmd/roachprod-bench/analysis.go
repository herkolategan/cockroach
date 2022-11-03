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

import "strings"

func extractBenchmarkResults(benchmarkOutput string) ([][]string, bool) {
	results := make([][]string, 0)
	buf := make([]string, 0)
	containsErrors := false
	var benchName string
	for _, line := range strings.Split(benchmarkOutput, "\n") {
		elems := strings.Fields(line)
		for index, s := range elems {
			if !containsErrors {
				containsErrors = strings.Contains(s, "FAIL") || strings.Contains(s, "panic")
			}
			if strings.HasPrefix(s, "Benchmark") && len(s) > 9 {
				benchName = s
			}
			if s == "ns/op" {
				er := elems[index-2:]
				buf = append(buf, er...)
				if benchName != "" {
					buf = append([]string{benchName}, buf...)
					results = append(results, buf)
				}
				buf = make([]string, 0)
				benchName = ""
			}
		}
	}
	return results, containsErrors
}
