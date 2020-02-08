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
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
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
	_, ok := os.LookupEnv("XML_OUTPUT_FILE")
	return ok
}

func isVerbose(args []string) bool {
	for _, s := range args {
		if s == "-test.v" {
			return true
		}
	}
	return false
}

// jsonEvent as encoded by the test2json package.
type jsonEvent struct {
	Time    *time.Time
	Action  string
	Package string
	Test    string
	Elapsed *float64
	Output  string
}

type testCase struct {
	state    string
	output   strings.Builder
	duration *float64
}

type testSuite struct {
	byName   map[string]*testCase
	duration *float64
	name     string
	events   []*jsonEvent
}

func (s *testSuite) named(name string) *testCase {
	if name == "" {
		return nil
	}
	if _, ok := s.byName[name]; !ok {
		s.byName[name] = &testCase{}
	}
	return s.byName[name]
}

func (s *testSuite) failed(name string) bool {
	if t, ok := s.byName[name]; ok {
		return t.state == "fail"
	}
	return false
}

type ingester struct {
	closers   []io.Closer
	converter io.WriteCloser
	tests     testSuite
	out       io.Writer
	verbose   bool
}

func newIngester(pkg string, verbose bool, out io.Writer) *ingester {
	r, w := io.Pipe()
	i := &ingester{
		closers: []io.Closer{w},
		tests: testSuite{
			byName: make(map[string]*testCase),
			name:   pkg,
		},
		out:     out,
		verbose: verbose,
	}
	go i.decodeJSONEvents(r)
	i.converter = NewConverter(w, pkg, Timestamp)
	return i
}

func (i *ingester) decodeJSONEvents(r *io.PipeReader) {
	dec := json.NewDecoder(r)
	for {
		var e jsonEvent
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			r.CloseWithError(err)
			break
		}
		i.handleJSONEvent(&e)
	}
}

func (i *ingester) Write(p []byte) (int, error) {
	if i.verbose {
		i.out.Write(p)
	}
	return i.converter.Write(p)
}

func (i *ingester) Close() error {
	for _, c := range i.closers {
		c.Close()
	}
	if !i.verbose {
		for _, e := range i.tests.events {
			if e.Test == "" || i.tests.failed(e.Test) {
				i.out.Write([]byte(e.Output))
			}
		}
	}
	return i.converter.Close()
}

func (i *ingester) handleJSONEvent(e *jsonEvent) {
	i.tests.events = append(i.tests.events, e)
	switch s := e.Action; s {
	case "run":
		if c := i.tests.named(e.Test); c != nil {
			c.state = s
		}
	case "output":
		if c := i.tests.named(e.Test); c != nil {
			c.output.WriteString(e.Output)
		}
	case "skip":
		if c := i.tests.named(e.Test); c != nil {
			c.output.WriteString(e.Output)
			c.state = s
			c.duration = e.Elapsed
		}
	case "fail":
		if c := i.tests.named(e.Test); c != nil {
			c.state = s
			c.duration = e.Elapsed
		} else {
			i.tests.duration = e.Elapsed
		}
	case "pass":
		if c := i.tests.named(e.Test); c != nil {
			c.duration = e.Elapsed
			c.state = s
		} else {
			i.tests.duration = e.Elapsed
		}
	}
}

func (i *ingester) writeXML(path string) error {
	xml, err := toXML(i.tests.name, i.tests.duration, i.tests.byName)
	if err != nil {
		return fmt.Errorf("error converting test output to xml: %s", err)
	}
	if err := ioutil.WriteFile(path, xml, 0664); err != nil {
		return fmt.Errorf("error writing test xml: %s", err)
	}
	return nil
}

func wrap(pkg string) error {
	ingester := newIngester(pkg, isVerbose(os.Args[1:]), os.Stdout)
	args := append([]string{"-test.v"}, os.Args[1:]...)
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "GO_TEST_WRAP=0")
	cmd.Stderr = os.Stderr
	cmd.Stdout = ingester
	err := cmd.Run()
	ingester.Close()
	if out, ok := os.LookupEnv("XML_OUTPUT_FILE"); ok {
		werr := ingester.writeXML(out)
		if werr != nil {
			if err != nil {
				return fmt.Errorf("error while generating testreport: %s, (error wrapping test execution: %s)", werr, err)
			}
			return fmt.Errorf("error while generating testreport: %s", werr)
		}
	}
	return err
}
