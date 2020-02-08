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
	"encoding/xml"
	"fmt"
	"path"
	"sort"
)

type xmlTestSuites struct {
	XMLName xml.Name       `xml:"testsuites"`
	Suites  []xmlTestSuite `xml:"testsuite"`
}

type xmlTestSuite struct {
	XMLName   xml.Name      `xml:"testsuite"`
	TestCases []xmlTestCase `xml:"testcase"`
	Errors    int           `xml:"errors,attr"`
	Failures  int           `xml:"failures,attr"`
	Skipped   int           `xml:"skipped,attr"`
	Tests     int           `xml:"tests,attr"`
	Time      string        `xml:"time,attr"`
	Name      string        `xml:"name,attr"`
}

type xmlTestCase struct {
	XMLName   xml.Name    `xml:"testcase"`
	Classname string      `xml:"classname,attr"`
	Name      string      `xml:"name,attr"`
	Time      string      `xml:"time,attr"`
	Failure   *xmlMessage `xml:"failure,omitempty"`
	Error     *xmlMessage `xml:"error,omitempty"`
	Skipped   *xmlMessage `xml:"skipped,omitempty"`
}

type xmlMessage struct {
	Message  string `xml:"message,attr"`
	Type     string `xml:"type,attr"`
	Contents string `xml:",chardata"`
}

func toXML(pkgName string, pkgDuration *float64, testcases map[string]*testCase) ([]byte, error) {
	cases := make([]string, 0, len(testcases))
	for k := range testcases {
		cases = append(cases, k)
	}
	sort.Strings(cases)
	suite := xmlTestSuite{
		Name: pkgName,
	}
	if pkgDuration != nil {
		suite.Time = fmt.Sprintf("%.3f", *pkgDuration)
	}
	for _, name := range cases {
		c := testcases[name]
		suite.Tests++
		newCase := xmlTestCase{
			Name:      name,
			Classname: path.Base(pkgName),
		}
		if c.duration != nil {
			newCase.Time = fmt.Sprintf("%.3f", *c.duration)
		}
		switch c.state {
		case "skip":
			suite.Skipped++
			newCase.Skipped = &xmlMessage{
				Message:  "Skipped",
				Contents: c.output.String(),
			}
		case "fail":
			suite.Failures++
			newCase.Failure = &xmlMessage{
				Message:  "Failed",
				Contents: c.output.String(),
			}
		case "pass":
			break
		default:
			suite.Errors++
			newCase.Error = &xmlMessage{
				Message:  "No pass/skip/fail event found for test",
				Contents: c.output.String(),
			}
		}
		suite.TestCases = append(suite.TestCases, newCase)
	}
	return xml.MarshalIndent(&xmlTestSuites{Suites: []xmlTestSuite{suite}}, "", "\t")
}
