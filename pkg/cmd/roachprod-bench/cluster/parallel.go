// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package cluster

import (
	"bytes"
	"context"
	"fmt"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachprod-bench"
	"io"
	"os"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachprod"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

type RemoteCommand struct {
	command  []string
	metadata interface{}
}

type RemoteResponse struct {
	RemoteCommand
	stdout   string
	stderr   string
	err      error
	duration time.Duration
}

func roachprodRunWithOutput(clusterName string, cmdArray []string) (string, string, error) {
	bufOut, bufErr := new(bytes.Buffer), new(bytes.Buffer)
	var stdout, stderr io.Writer
	if *main.flagVerbose {
		stdout = io.MultiWriter(os.Stdout, bufOut)
		stderr = io.MultiWriter(os.Stderr, bufErr)
	} else {
		stdout = bufOut
		stderr = bufErr
	}
	err := roachprod.Run(context.Background(), main.l, clusterName, "", "", false, stdout, stderr, cmdArray)
	return bufOut.String(), bufErr.String(), err
}

func remoteWorker(clusterNode string, receive chan remoteCommand, response chan remoteResponse) {
	for {
		command := <-receive
		if command.command == nil {
			return
		}
		start := timeutil.Now()
		stdout, stderr, err := roachprodRunWithOutput(clusterNode, command.command)
		duration := timeutil.Since(start)
		response <- remoteResponse{command, stdout, stderr, err, duration}
	}
}

func ExecuteRemoteCommands(
	cluster string,
	commands []remoteCommand,
	numNodes int,
	failFast bool,
	callback func(response remoteResponse),
) {
	workChannel := make(chan remoteCommand, numNodes)
	responseChannel := make(chan remoteResponse, numNodes)

	for idx := 1; idx <= numNodes; idx++ {
		go remoteWorker(fmt.Sprintf("%s:%d", cluster, idx), workChannel, responseChannel)
	}

	responsesRemaining := 0
out:
	for _, command := range commands {
		workChannel <- command
		responsesRemaining++

		select {
		case response := <-responseChannel:
			responsesRemaining--
			callback(response)
			if response.err != nil && failFast {
				break out
			}
		default:
		}
	}

	for responsesRemaining > 0 {
		response := <-responseChannel
		responsesRemaining--
		callback(response)
	}

	close(workChannel)
	close(responseChannel)
}
