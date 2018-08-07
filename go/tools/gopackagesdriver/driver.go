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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bazelbuild/rules_go/go/tools/gopackagesdriver/build_event_stream"
	"github.com/golang/protobuf/proto"
	"golang.org/x/tools/go/packages"
)

var programName = filepath.Base(os.Args[0])

const usageFmt = `Usage: %s list [-deps] [-test] [-export]
		[-buildflag=flag...] -- patterns...

Bazel gopackagesdriver gathers metadata about packages in a Bazel workspace
and prints that information on stdout in json format. gopackagesdriver is
intended to be invoked by golang.org/x/tools/go/packages when the
GOPACKAGESDRIVER environment variable is set.

`

func main() {
	log.SetPrefix(programName + ": ")
	log.SetFlags(0)
	var cmd string
	if len(os.Args) >= 2 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "list":
		resp, err := list(os.Args[2:])
		if err != nil {
			log.Fatal(err)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			log.Fatal(err)
		}

	case "help", "-h", "-help":
		list([]string{"-h"})

	default:
		log.Fatalf("%s: unknown command. Run '%s help' for usage.", cmd, programName)
	}
}

// copied from packages
type driverResponse struct {
	Roots    []string `json:",omitempty"`
	Packages []*packages.Package
}

func list(args []string) (driverResponse, error) {
	fs := flag.NewFlagSet(programName, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, usageFmt, programName)
		fs.PrintDefaults()
	}
	var needTests, needExport, needDeps bool
	var buildFlags multiFlag
	fs.BoolVar(&needTests, "test", false, "Whether test packages should be included")
	fs.BoolVar(&needExport, "export", false, "Whether export data should be built")
	fs.BoolVar(&needDeps, "deps", false, "Whether information about dependencies is needed")
	fs.Var(&buildFlags, "buildflag", "Additional flags to pass to Bazel (may be repeated)")
	if err := fs.Parse(args); err != nil {
		return driverResponse{}, err
	}

	patterns := fs.Args()

	binDir, workspaceDir, execDir, err := getBazelDirs()
	if err == noWorkspaceError {
		return listFallback(needTests, needExport, needDeps, buildFlags, patterns)
	} else if err != nil {
		return driverResponse{}, err
	}

	targets, err := queryTargets(buildFlags, patterns)
	if err != nil {
		return driverResponse{}, err
	}

	files, err := buildPkgFiles(needDeps, needExport, needTests, buildFlags, targets)
	if err != nil {
		return driverResponse{}, err
	}
	for i := range files {
		files[i] = filepath.Join(binDir, files[i])
	}

	pkgs, err := loadPkgFiles(files)
	if err != nil {
		return driverResponse{}, err
	}
	for _, pkg := range pkgs {
		absPkgPaths(pkg, workspaceDir, execDir)
		pkgs = append(pkgs, pkg)
	}

	resp := driverResponse{
		Roots:    targets,
		Packages: pkgs,
	}
	return resp, nil
}

func getBazelDirs() (binDir, workspaceDir, execDir string, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", "", "", err
	}
	for {
		_, err := os.Stat(filepath.Join(dir, "WORKSPACE"))
		if err == nil {
			workspaceDir = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", "", noWorkspaceError
		}
		dir = parent
	}

	out, err := exec.Command("bazel", "info").Output()
	if err != nil {
		return "", "", "", err
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		var key, value string
		sep := ": "
		if i := strings.Index(line, sep); i < 0 {
			continue
		} else {
			key = line[:i]
			value = line[i+len(sep):]
		}
		switch key {
		case "bazel-bin":
			binDir = value
		case "execution_root":
			execDir = value
		}
	}
	return binDir, workspaceDir, execDir, nil
}

func queryTargets(buildFlags, patterns []string) ([]string, error) {
	args := []string{"query"}
	args = append(args, buildFlags...)
	args = append(args, "--")
	args = append(args, patterns...)
	cmd := exec.Command("bazel", args...)
	buf := &bytes.Buffer{}
	cmd.Stdout = buf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var targets []string
	data := buf.Bytes()
	i := bytes.IndexByte(data, '\n')
	for i >= 0 {
		target := string(data[:i])
		data = data[i+1:]
		i = bytes.IndexByte(data, '\n')
		if len(target) > 0 && target[len(target)-1] == '\r' {
			target = target[:len(target)-1]
		}
		targets = append(targets, target)
	}
	return targets, nil
}

const (
	aspectFileName  = "@io_bazel_rules_go//go/tools/gopackagesdriver:aspect.bzl"
	outputGroupName = "gopackagesdriver"
)

func buildPkgFiles(needDeps, needExport, needTest bool, buildFlags, targets []string) ([]string, error) {
	logFile, err := ioutil.TempFile("", "gopackagesdriver")
	if err != nil {
		return nil, err
	}
	logPath := logFile.Name()
	logFile.Close()
	defer os.Remove(logPath)

	var b strings.Builder
	b.WriteString(aspectFileName)
	b.WriteString("%gopackagesdriver_")
	if needDeps {
		b.WriteString("deps_")
	}
	if needExport {
		b.WriteString("export_")
	}
	if needTest {
		b.WriteString("test_")
	}
	b.WriteString("aspect")
	aspectName := b.String()
	args := []string{
		"build",
		"-s",
		"--spawn_strategy=standalone",
		"--aspects=" + aspectName,
		"--output_groups=" + outputGroupName,
		"--build_event_binary_file=" + logPath,
	}
	args = append(args, buildFlags...)
	args = append(args, "--")
	args = append(args, targets...)
	cmd := exec.Command("bazel", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	logData, err := ioutil.ReadFile(logPath)
	if err != nil {
		return nil, err
	}
	var event build_event_stream.BuildEvent
	setToFiles := make(map[string][]string)
	setToSets := make(map[string][]string)
	var rootSets []string
	for len(logData) > 0 {
		size, n := proto.DecodeVarint(logData)
		if n == 0 {
			return nil, err
			break
		}
		logData = logData[n:]
		if err := proto.Unmarshal(logData[:size], &event); err != nil {
			return nil, err
		}
		logData = logData[size:]

		if id := event.GetId().GetTargetCompleted(); id != nil {
			if id.GetAspect() != aspectName {
				continue
			}
			completed := event.GetCompleted()
			if !completed.GetSuccess() {
				return nil, fmt.Errorf("%s did not build successfully", id.GetLabel())
			}
			for _, g := range completed.GetOutputGroup() {
				if g.GetName() != outputGroupName {
					continue
				}
				for _, s := range g.GetFileSets() {
					if setId := s.GetId(); setId != "" {
						rootSets = append(rootSets, setId)
					}
				}
			}
			continue
		}

		if id := event.GetId().GetNamedSet(); id != nil {
			files := event.GetNamedSetOfFiles().GetFiles()
			fileNames := make([]string, len(files))
			for i, f := range files {
				fileNames[i] = f.GetName()
			}
			setToFiles[id.GetId()] = fileNames
			sets := event.GetNamedSetOfFiles().GetFileSets()
			setIds := make([]string, len(sets))
			for i, s := range sets {
				setIds[i] = s.GetId()
			}
			setToSets[id.GetId()] = setIds
			continue
		}
	}

	files := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(setId string) {
		if visited[setId] {
			return
		}
		visited[setId] = true
		for _, f := range setToFiles[setId] {
			files[f] = true
		}
		for _, s := range setToSets[setId] {
			visit(s)
		}
	}
	for _, s := range rootSets {
		visit(s)
	}
	sortedFiles := make([]string, 0, len(files))
	for f := range files {
		sortedFiles = append(sortedFiles, f)
	}
	sort.Strings(sortedFiles)
	return sortedFiles, nil
}

func loadPkgFiles(paths []string) ([]*packages.Package, error) {
	var pkgs []*packages.Package
	for _, path := range paths {
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			dirPkgs, err := loadPkgDir(path)
			if err != nil {
				return nil, err
			}
			pkgs = append(pkgs, dirPkgs...)
		} else {
			pkg, err := loadPkgFile(path)
			if err != nil {
				return nil, err
			}
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs, nil
}

func loadPkgFile(pkgFilePath string) (*packages.Package, error) {
	pkgData, err := ioutil.ReadFile(pkgFilePath)
	if err != nil {
		return nil, err
	}
	pkg := &packages.Package{}
	if err := json.Unmarshal(pkgData, pkg); err != nil {
		return nil, err
	}
	return pkg, nil
}

func loadPkgDir(pkgDirPath string) ([]*packages.Package, error) {
	var pkgs []*packages.Package
	err := filepath.Walk(pkgDirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		pkg, err := loadPkgFile(path)
		if err != nil {
			return err
		}
		pkgs = append(pkgs, pkg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pkgs, nil
}

func absPkgPaths(pkg *packages.Package, workspaceDir, execDir string) {
	for i := range pkg.GoFiles {
		pkg.GoFiles[i] = filepath.Join(workspaceDir, pkg.GoFiles[i])
	}
	for i := range pkg.CompiledGoFiles {
		pkg.CompiledGoFiles[i] = filepath.Join(workspaceDir, pkg.CompiledGoFiles[i])
	}
	for i := range pkg.OtherFiles {
		pkg.OtherFiles[i] = filepath.Join(workspaceDir, pkg.OtherFiles[i])
	}
	if pkg.ExportFile != "" {
		pkg.ExportFile = filepath.Join(workspaceDir, pkg.ExportFile)
	}
}

var noWorkspaceError = errors.New("working directory is outside any Bazel workspace")

func listFallback(needTests, needExport, needDeps bool, buildFlags []string, patterns []string) (driverResponse, error) {
	cfg := &packages.Config{
		BuildFlags: buildFlags,
		Tests:      needTests,
		Env:        append(os.Environ(), "GOPACKAGESDRIVER=off"),
	}
	switch {
	case needExport:
		cfg.Mode = packages.LoadTypes
	case needDeps:
		cfg.Mode = packages.LoadImports
	default:
		cfg.Mode = packages.LoadFiles
	}
	roots, err := packages.Load(cfg, patterns...)
	if err != nil {
		return driverResponse{}, nil
	}
	resp := driverResponse{}
	seen := make(map[*packages.Package]bool)
	var visit func(*packages.Package)
	visit = func(pkg *packages.Package) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		resp.Packages = append(resp.Packages, pkg)
		for _, ipkg := range pkg.Imports {
			visit(ipkg)
		}
	}
	for _, pkg := range roots {
		resp.Roots = append(resp.Roots, pkg.ID)
		visit(pkg)
	}
	return resp, nil
}

// multiFlag allows repeated string flags to be collected into a slice
type multiFlag []string

func (m *multiFlag) String() string {
	if len(*m) == 0 {
		return ""
	}
	return fmt.Sprint(*m)
}

func (m *multiFlag) Set(v string) error {
	(*m) = append(*m, v)
	return nil
}
