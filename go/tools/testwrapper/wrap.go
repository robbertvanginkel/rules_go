// Copyright 2020 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
)

// testWrapperAbnormalExit is used by the testwrapper to indicate the child
// process exitted without an exit code (for example being killed by a signal).
// We use 6, in line with Bazel's RUN_FAILURE.
const testWrapperAbnormalExit = 6

func shouldWrap() bool {
	if wrapEnv, ok := os.LookupEnv("GO_TEST_WRAP"); ok {
		wrap, err := strconv.ParseBool(wrapEnv)
		if err != nil {
			log.Fatalf("invalid value for GO_TEST_WRAP: %q", wrapEnv)
		}
		return wrap
	}
	if _, ok := os.LookupEnv("XML_OUTPUT_FILE"); ok {
		return true
	}
	return false
}

func wrap() error {
	var stdoutBuf bytes.Buffer
	args := append([]string{"-test.v"}, os.Args[1:]...)
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "GO_TEST_WRAP=0")
	cmd.Stderr = os.Stderr
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutBuf)
	err := cmd.Run()
	// do something with stdoutBuf
	return err
}
