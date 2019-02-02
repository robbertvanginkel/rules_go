// Copyright 2018 The Bazel Authors. All rights reserved.
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

// Program 'package' reads the sources for a Go package and writes a
// packages.Package object to a file, encoded in JSON. Actions that execute
// this program are emitted in gopackagesdriver/aspect.bzl. The whole thing
// is driven by gopackagesdriver.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

var programName = filepath.Base(os.Args[0])

func main() {
	log.SetPrefix(programName + ": ")
	log.SetFlags(0)
	var cmd string
	if len(os.Args) >= 2 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "pkg":
		if err := buildPackage(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "stdlib":
		if err := buildStdlibPackages(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatalf("unknown command: %q", cmd)
	}
}

type archive struct {
	label, importPath, importMap, file string
}

func buildPackage(args []string) error {
	args, err := readParamsFiles(args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(programName, flag.ExitOnError)
	var goSrcs, otherSrcs, origSrcs multiFlag
	var archives archiveMultiFlag
	var id, importPath, importMap, pkgFilePath, outPath string
	var testfilterName string
	goenv := envFlags(flags)
	flags.StringVar(&id, "id", "", "The package ID reported back to the API")
	flags.StringVar(&importPath, "importpath", "", "The source import path for the package")
	flags.StringVar(&importMap, "importmap", "", "The package path for the package")
	flags.StringVar(&pkgFilePath, "file", "", "The compiled package file")
	flags.Var(&goSrcs, "go_src", "A source file that would be passed to the compiler if it satisfies build constraints")
	flags.Var(&otherSrcs, "other_src", "A source file that would be passed to the compiler if it satisfies build constraints")
	flags.Var(&origSrcs, "orig_src", "An original source file, without any cgo / cover processing.")
	flags.StringVar(&testfilterName, "testfilter", "off", "Controls test package filtering")
	flags.Var(&archives, "arc", "Label, import path, package path, and file name of a direct dependency, separated by '='")
	flags.StringVar(&outPath, "o", "", "The output package file to write")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlags(); err != nil {
		return err
	}
	if id == "" {
		return errors.New("-id not set")
	}
	if importPath == "" {
		return errors.New("-importpath not set")
	}
	if importMap == "" {
		return errors.New("-importmap not set")
	}
	if outPath == "" {
		return errors.New("-o not set")
	}
	testfilter, err := testfilterFromString(testfilterName)
	if err != nil {
		return err
	}

	pkg := &packages.Package{
		ID:         id,
		PkgPath:    importMap,
		ExportFile: pkgFilePath,
		Imports:    map[string]*packages.Package{},
	}

	importPathToLabel := make(map[string]string)
	for _, arc := range archives {
		importPathToLabel[arc.importPath] = arc.label
	}

	pkg.GoFiles = make([]string, 0, len(origSrcs))
	for _, src := range origSrcs {
		if m, err := readGoMetadata(build.Default, src, false); err != nil {
			return err
		} else if m.matched && testfilter(m) {
			pkg.GoFiles = append(pkg.GoFiles, src)
		}
	}
	pkg.OtherFiles = make([]string, 0, len(otherSrcs))
	for _, src := range otherSrcs {
		if m, err := readGoMetadata(build.Default, src, false); err != nil {
			return err
		} else if m.matched {
			pkg.OtherFiles = append(pkg.OtherFiles, src)
		}
	}
	pkg.CompiledGoFiles = make([]string, 0, len(goSrcs))
	for _, src := range goSrcs {
		m, err := readGoMetadata(build.Default, src, true)
		if err != nil {
			return err
		} else if !m.matched || !testfilter(m) {
			continue
		}
		pkg.CompiledGoFiles = append(pkg.CompiledGoFiles, src)
		if pkg.Name == "" {
			pkg.Name = m.pkg
		}
		for _, imp := range m.imports {
			if label, ok := importPathToLabel[imp]; !ok {
				pkg.Imports[imp] = &packages.Package{ID: stdlibPkgId(imp)}
			} else {
				pkg.Imports[imp] = &packages.Package{ID: label}
			}
		}
	}
	if pkg.Name == "" {
		pkg.Name = "empty"
	}

	return writeJsonFile(pkg, outPath)
}

func buildStdlibPackages(args []string) error {
	args, err := readParamsFiles(args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(programName, flag.ExitOnError)
	goenv := envFlags(flags)
	var goExe, outPath string
	flags.StringVar(&goExe, "go", "", "Path to go executable")
	flags.StringVar(&outPath, "o", "", "Path to directory of json package files to write")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlags(); err != nil {
		return err
	}
	if goExe == "" {
		return errors.New("-go not set")
	}
	if outPath == "" {
		return errors.New("-o not set")
	}

	goExe = abs(goExe)
	os.Setenv("PATH", filepath.Dir(goExe))
	os.Setenv("GOROOT", abs(os.Getenv("GOROOT")))
	os.Setenv("GOPACKAGESDRIVER", "off")

	// go list only works with gocache. We cannot remove the go cache because go list's
	// output will refer to generated files in the cache.
	cachePath := filepath.Join(outPath, ".gocache")
	os.Setenv("GOCACHE", cachePath)

	// Make sure we have an absolute path to the C compiler.
	os.Setenv("CC", abs(os.Getenv("CC")))

	cfg := &packages.Config{
		Mode: packages.LoadAllSyntax,
	}
	pkgs, err := packages.Load(cfg, "std")
	if err != nil {
		return err
	}
	for _, pkg := range pkgs {
		pkg.ID = "@io_bazel_rules_go//:stdlib%" + pkg.ID
	}
	for _, pkg := range pkgs {
		outName := filepath.FromSlash(pkg.PkgPath) + ".json"
		pkgOutPath := filepath.Join(outPath, outName)
		if err := writeJsonFile(pkg, pkgOutPath); err != nil {
			return err
		}
	}
	return nil
}

func writeJsonFile(data interface{}, outPath string) (err error) {
	if err := os.MkdirAll(filepath.Dir(outPath), 0777); err != nil {
		return err
	}
	outFile, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := outFile.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()
	enc := json.NewEncoder(outFile)
	return enc.Encode(data)
}

func stdlibPkgId(importPath string) string {
	return "@io_bazel_rules_go//:stdlib%" + importPath
}

type archiveMultiFlag []archive

func (m *archiveMultiFlag) String() string {
	if m == nil || len(*m) == 0 {
		return ""
	}
	return fmt.Sprint(*m)
}

func (m *archiveMultiFlag) Set(v string) error {
	parts := strings.Split(v, "=")
	if len(parts) != 4 {
		return fmt.Errorf("badly formed -arc flag: %s", v)
	}
	*m = append(*m, archive{
		label:      parts[0],
		importPath: parts[1],
		importMap:  parts[2],
		file:       parts[3],
	})
	return nil
}

func testfilterFromString(s string) (func(*goMetadata) bool, error) {
	switch s {
	case "off":
		return func(f *goMetadata) bool { return true }, nil
	case "only":
		return func(f *goMetadata) bool { return strings.HasSuffix(f.pkg, "_test") }, nil
	case "exclude":
		return func(f *goMetadata) bool { return !strings.HasSuffix(f.pkg, "_test") }, nil
	default:
		return nil, fmt.Errorf("Invalid test filter %q", s)
	}
}
